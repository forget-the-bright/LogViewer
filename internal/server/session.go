package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"logviewer/internal/metrics"
)

// sessionRingBytes 断线宽限期内环形缓冲的最大字节数（约 2MB）。
// 超出后淘汰最旧的批次；重连时若 lastSeq 落在被淘汰区间，前端会收到缺口提示。
const sessionRingBytes = 2 * 1024 * 1024

// ringEntry 是环形缓冲中的一批日志及其单调递增序号。
type ringEntry struct {
	seq  uint64
	data string
}

// viewSession 把一次 follow 跟踪任务从单个 WebSocket 连接上解耦：
// 进程持续运行，输出写入有界环形缓冲；WS 断线后在宽限期内保留会话，
// 重连时按 lastSeq 补发缺口，避免日志丢失或重头再来。
//
// 并发模型：
//   - mu 保护环形缓冲、序号、client 指针与生命周期状态；
//   - writeMu 串行化所有发往当前 client 的网络写（实时写与重连补发不会交错乱序）；
//   - outFn 先在 mu 下入缓冲并取序号，再在 writeMu 下写 client，
//     因此慢客户端只会阻塞写，不会阻塞进程读取（缓冲照常累积）。
type viewSession struct {
	id       string
	hostName string
	abs      string
	msg      *wsMessage
	grace    time.Duration

	mu         sync.Mutex
	ring       []ringEntry
	ringBytes  int
	oldestSeq  uint64 // ring[0] 的序号；缓冲为空时为 0
	nextSeq    uint64 // 下一个待分配序号（从 1 开始）
	client     *wsClient
	closed     bool
	procID     uint64
	graceTimer *time.Timer
	runGen     uint64
	runCancel  context.CancelFunc

	writeMu sync.Mutex
	reg     *sessionRegistry
}

// sessionRegistry 按 sessionID 索引存活的会话。
type sessionRegistry struct {
	mu  sync.Mutex
	m   map[string]*viewSession
	srv *Server
}

func newSessionRegistry(srv *Server) *sessionRegistry {
	return &sessionRegistry{m: map[string]*viewSession{}, srv: srv}
}

func (r *sessionRegistry) put(s *viewSession) {
	r.mu.Lock()
	r.m[s.id] = s
	r.mu.Unlock()
}

func (r *sessionRegistry) get(id string) *viewSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

func (r *sessionRegistry) remove(s *viewSession) {
	r.mu.Lock()
	if cur := r.m[s.id]; cur == s {
		delete(r.m, s.id)
	}
	r.mu.Unlock()
}

// randHex 生成 n 字节随机量的十六进制会话标识。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 随机源失败属极端异常；退回时间戳避免崩溃（会话碰撞概率可忽略）。
		return uitoa(uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b)
}

func newSessionID() string { return randHex(8) }

// pushRing 追加一批数据并按字节上限淘汰最旧批次。调用方须持 sess.mu。
func (sess *viewSession) pushRing(seq uint64, data string) {
	sess.ring = append(sess.ring, ringEntry{seq: seq, data: data})
	sess.ringBytes += len(data)
	for sess.ringBytes > sessionRingBytes && len(sess.ring) > 1 {
		drop := sess.ring[0]
		sess.ring = sess.ring[1:]
		sess.ringBytes -= len(drop.data)
	}
	if len(sess.ring) > 0 {
		sess.oldestSeq = sess.ring[0].seq
	} else {
		sess.oldestSeq = 0
	}
}

// entriesAfter 返回 seq 大于 lastSeq 的缓冲批次；gap 表示 lastSeq 落在已淘汰区间。
// 返回的是切片拷贝，调用方可在不持锁的情况下安全遍历。调用方须持 sess.mu。
func (sess *viewSession) entriesAfter(lastSeq uint64) (entries []ringEntry, gap bool) {
	if len(sess.ring) == 0 {
		return nil, false
	}
	if lastSeq < sess.oldestSeq {
		gap = true // 一些批次已被淘汰，无法补发
	}
	for _, e := range sess.ring {
		if e.seq > lastSeq {
			entries = append(entries, e)
		}
	}
	return entries, gap
}

// onStdout 是进程 stdout 回调：分配序号、入环形缓冲，若已连接则实时下发。
func (sess *viewSession) onStdout(batch string) {
	metrics.IncLogBytes(len(batch))

	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return
	}
	seq := sess.nextSeq
	sess.nextSeq++
	sess.pushRing(seq, batch)
	cl := sess.client
	sess.mu.Unlock()

	if cl == nil {
		return // 断线宽限期：仅入缓冲，不写网络
	}
	sess.writeLog(cl, seq, batch)
}

// writeLog 在 writeMu 下把一批日志以带序号的帧写入 client。
func (sess *viewSession) writeLog(cl *wsClient, seq uint64, batch string) {
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	sess.reg.srv.sendText(cl, `{"type":"log","seq":`+uitoa(seq)+`,"data":`+jsonQuote(batch)+`}`)
}

// onStderr 转发 stderr 分类结果给当前连接；断线期间丢弃（多为轮转等良性噪声）。
func (sess *viewSession) onStderr(line string) {
	class := classifyStderr(line)
	if class == stderrBenign {
		return
	}
	sess.mu.Lock()
	cl := sess.client
	sess.mu.Unlock()
	if cl == nil {
		return
	}
	switch class {
	case stderrRotate:
		sess.reg.srv.sendNotice(cl, "rotate")
	case stderrTruncate:
		sess.reg.srv.sendNotice(cl, "truncate")
	default:
		sess.reg.srv.sendError(cl, strings.TrimSpace(trimLine(line)))
	}
}

// onDone 进程自然退出回调：通知当前连接 stopped 并从注册表移除。
func (sess *viewSession) onDone() {
	sess.mu.Lock()
	sess.procID = 0
	cl := sess.client
	already := sess.closed
	if !already {
		sess.closed = true
	}
	sess.mu.Unlock()

	sess.reg.srv.refreshProcMetric()
	sess.reg.remove(sess)
	if cl != nil && !already {
		sess.reg.srv.sendStatus(cl, "stopped")
	}
}

// sendErrorIfAttached 在有连接时下发错误；否则仅记录。
func (sess *viewSession) sendErrorIfAttached(msg string) {
	sess.mu.Lock()
	cl := sess.client
	sess.mu.Unlock()
	if cl != nil {
		sess.reg.srv.sendError(cl, msg)
	} else {
		slog.Warn("会话启动失败但无连接附着", "session", sess.id, "host", sess.hostName, "err", msg)
	}
}

// attach 把新连接绑定到会话：取消宽限定时器，按 lastSeq 补发缺口，随后恢复实时下发。
// 返回 true 表示成功接管；false 表示会话已失效（进程已退出/被销毁），调用方应回退到全量重启。
//
// 锁序：先 writeMu 再 mu。onStdout 在 mu 下取 client 后释放 mu，再取 writeMu 写实时帧。
// 若 attach 先 mu 后 writeMu，在两次加锁之间 onStdout 可能拿到新 client 并先写一条实时帧，
// 导致补发帧（seq < nextSeq）排在实时帧（seq >= nextSeq）之后，客户端日志乱序。
// 先持 writeMu 可在整个补发期间阻塞 onStdout 的 writeLog，保证补发先于实时到达。
func (sess *viewSession) attach(cl *wsClient, lastSeq uint64) bool {
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()

	sess.mu.Lock()
	if sess.closed || sess.procID == 0 {
		sess.mu.Unlock()
		return false
	}
	if sess.graceTimer != nil {
		sess.graceTimer.Stop()
		sess.graceTimer = nil
	}
	entries, gap := sess.entriesAfter(lastSeq)
	sess.client = cl
	nextSeq := sess.nextSeq
	sess.mu.Unlock()

	// 在 writeMu 下整体补发：期间任何实时 onStdout 都会阻塞到补发完成后，
	// 保证补发帧（seq ≤ nextSeq-1）全部先于实时帧（seq ≥ nextSeq）到达。
	if gap {
		sess.reg.srv.sendText(cl, `{"type":"notice","kind":"gap"}`)
	}
	for _, e := range entries {
		sess.reg.srv.sendText(cl, `{"type":"log","seq":`+uitoa(e.seq)+`,"data":`+jsonQuote(e.data)+`}`)
	}
	slog.Info("会话重连补发", "session", sess.id, "host", sess.hostName,
		"lastSeq", lastSeq, "replayed", len(entries), "gap", gap, "nextSeq", nextSeq)
	return true
}

// detach 在 WS 断线时解绑当前连接并启动宽限定时器。宽限期内进程继续运行、
// 输出进入环形缓冲；超时则销毁会话。
func (sess *viewSession) detach() {
	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return
	}
	sess.client = nil
	if sess.graceTimer != nil {
		sess.graceTimer.Stop()
	}
	d := sess.grace
	sess.graceTimer = time.AfterFunc(d, func() {
		slog.Info("会话宽限期到期，销毁会话", "session", sess.id, "host", sess.hostName,
			"grace", d.String())
		sess.destroy()
	})
	sess.mu.Unlock()
	slog.Info("WS 断线，会话进入宽限期", "session", sess.id, "host", sess.hostName,
		"grace", d.String())
}

// destroy 立即终止会话：取消等待、杀进程、从注册表移除。供 stop/切换/宽限到期调用。
func (sess *viewSession) destroy() {
	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return
	}
	sess.closed = true
	if sess.runCancel != nil {
		sess.runCancel()
	}
	procID := sess.procID
	sess.procID = 0
	sess.client = nil
	if sess.graceTimer != nil {
		sess.graceTimer.Stop()
		sess.graceTimer = nil
	}
	sess.mu.Unlock()

	if procID != 0 {
		sess.reg.srv.procs.Stop(procID)
	}
	sess.reg.remove(sess)
	sess.reg.srv.refreshProcMetric()
}

// beginGen 开新一代文件等待任务，取消上一代。返回该代的 ctx 与 gen。
func (sess *viewSession) beginGen() (uint64, context.Context) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.runCancel != nil {
		sess.runCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	sess.runGen++
	sess.runCancel = cancel
	return sess.runGen, ctx
}

// isCurrentGen 判断 gen 是否仍是当前代且会话未关闭。
func (sess *viewSession) isCurrentGen(gen uint64) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return !sess.closed && sess.runGen == gen
}

// start 启动会话：校验参数、解析路径、处理文件等待，最终 launch 进程。
func (sess *viewSession) start() {
	srv := sess.reg.srv
	h, err := srv.hosts.Get(sess.hostName)
	if err != nil {
		sess.sendErrorIfAttached("内部错误：" + err.Error())
		sess.destroy()
		return
	}
	if err := sess.msg.Config.Validate(); err != nil {
		sess.sendErrorIfAttached("参数错误: "+err.Error())
		sess.destroy()
		return
	}
	abs, err := h.ResolvePath(sess.msg.FilePath)
	if err != nil {
		sess.sendErrorIfAttached(err.Error())
		sess.destroy()
		return
	}
	sess.abs = abs

	info, statErr := h.Stat(abs)
	if statErr != nil {
		if os.IsNotExist(statErr) && sess.msg.Config.FollowTail {
			sess.mu.Lock()
			cl := sess.client
			sess.mu.Unlock()
			if cl != nil {
				srv.sendStatus(cl, "waiting")
			}
			gen, ctx := sess.beginGen()
			go sess.waitForFile(ctx, gen)
			return
		}
		sess.sendErrorIfAttached("无法访问文件: " + statErr.Error())
		sess.destroy()
		return
	}
	if info != nil && info.IsDir() {
		sess.sendErrorIfAttached("所选为目录，请选择日志文件")
		sess.destroy()
		return
	}
	sess.launch()
}

// waitForFile 轮询直到文件出现或被取消，然后 launch。
func (sess *viewSession) waitForFile(ctx context.Context, gen uint64) {
	t := time.NewTicker(filePollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h, err := sess.reg.srv.hosts.Get(sess.hostName)
		if err != nil {
			return
		}
		info, err := h.Stat(sess.abs)
		if err == nil && info != nil && !info.IsDir() {
			if sess.isCurrentGen(gen) {
				sess.launch()
			}
			return
		}
	}
}

// launch 构建命令并启动进程，把输出路由到会话缓冲/连接。
func (sess *viewSession) launch() {
	srv := sess.reg.srv
	h, err := srv.hosts.Get(sess.hostName)
	if err != nil {
		sess.sendErrorIfAttached("内部错误：" + err.Error())
		sess.destroy()
		return
	}
	procID, err := srv.runView(h, sess.abs, sess.msg, sess.onStdout, sess.onStderr, sess.onDone)
	if err != nil {
		sess.sendErrorIfAttached(viewErrMsg(err))
		sess.destroy()
		return
	}
	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		srv.procs.Stop(procID)
		return
	}
	sess.procID = procID
	cl := sess.client
	nextSeq := sess.nextSeq
	sess.mu.Unlock()

	srv.refreshProcMetric()
	if cl != nil {
		// 下发 running 状态，带 sessionID 与当前序号（前端据此记录 lastSeq）。
		srv.sendText(cl, `{"type":"status","status":"running","sessionID":`+
			jsonQuote(sess.id)+`,"seq":`+uitoa(nextSeq)+`}`)
	}
}

// ---------- 辅助 ----------

func uitoa(u uint64) string {
	if u == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}
