package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
	"logviewer/internal/host"
	"logviewer/internal/metrics"
)

// wsClient 每个连接的状态：写锁 + 当前子进程 + 所属机器。
//
// runGen/runCancel 管理"本次查看任务"的生命周期：follow 模式下文件可能暂不存在，
// startSession 会启动一个后台轮询等待文件出现；用户在等待期间点击停止/切换文件
// 会 cancel 掉这一代等待。runGen 用于让旧一代的等待/启动回调失效，避免"等待结束
// 后把进程错误地挂到新一代任务上"。
type wsClient struct {
	conn     *websocket.Conn
	wmu      sync.Mutex
	host     host.Host
	hostName string

	runMu  sync.Mutex
	procID uint64 // 仅静态模式（一次性读取）使用；follow 模式由 viewSession 管理

	// session 是当前连接附着的 follow 会话（断线宽限/补齐）。
	// 静态模式为 nil。
	session *viewSession
}

// filePollInterval 等待暂不存在的日志文件时的轮询间隔。
const filePollInterval = 500 * time.Millisecond

// wsMessage 上行指令
type wsMessage struct {
	Action    string           `json:"action"`
	FilePath  string           `json:"filePath"`
	Config    config.LogConfig `json:"config"`
	SessionID string           `json:"sessionID"`
	LastSeq   uint64           `json:"lastSeq"`
}

const (
	// wsPingInterval 服务端主动发 ping 的间隔
	wsPingInterval = 30 * time.Second
	// wsPongWait 客户端 pong 超时（超过此时长无消息则判定连接已死）
	wsPongWait = 90 * time.Second
	// wsWriteWait 写消息的超时
	wsWriteWait = 10 * time.Second
)

// handleWS 处理 WebSocket 连接。host 通过 query 参数 ?host= 指定。
func (s *Server) handleWS(c *gin.Context) {
	// WebSocket 不能走普通 JSON 中间件，这里在 Upgrade 前显式校验会话 cookie。
	if s.auth.Enabled() {
		ck, err := c.Cookie(sessionCookie)
		if err != nil || !s.auth.validate(ck) {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
	}
	hostName := c.Query("host")
	h, err := s.hosts.Get(hostName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	upg := s.upgrader()
	conn, err := upg.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	cl := &wsClient{conn: conn, host: h, hostName: hostName}
	s.registerClient(cl, hostName)

	defer func() {
		// WS 断开：follow 会话进入宽限期（不杀进程，缓冲日志等重连 attach）；
		// 静态进程无补齐意义，直接回收。
		s.detachClient(cl, false)
		s.unregisterClient(cl)
		conn.Close()
	}()
	metrics.WSInc(hostName)
	defer metrics.WSDec(hostName)

	conn.SetReadLimit(1 << 20)
	conn.SetReadDeadline(time.Now().Add(wsPongWait))

	// 收到客户端任意消息（含 pong）时续期读超时
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})

	// 服务端定期发 ping，检测半开连接
	pingTicker := time.NewTicker(wsPingInterval)
	defer pingTicker.Stop()
	go func() {
		for range pingTicker.C {
			cl.wmu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait))
			cl.wmu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		// 收到消息续期读超时
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		var msg wsMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			s.sendError(cl, "消息解析失败: "+err.Error())
			continue
		}
		switch msg.Action {
		case "start":
			s.startSession(cl, &msg)
		case "attach":
			s.attachSession(cl, &msg)
		case "stop":
			s.stopSession(cl)
			s.sendStatus(cl, "stopped")
		case "ping":
			s.sendText(cl, `{"type":"status","status":"alive"}`)
		}
	}
}

// startSession 启动一次日志查看。
//
// 跟踪（follow）模式走 viewSession：会话与连接解耦，断线进入宽限期并缓冲日志，
// 重连可按序号补齐（详见 session.go）。静态模式仍是一次性读取，进程绑定到当前
// 连接，断连即杀（无补齐意义）。
func (s *Server) startSession(cl *wsClient, msg *wsMessage) {
	// 先停掉本连接上一代任务（静态进程 或 已附着会话）。
	s.detachClient(cl, true)

	if msg.Config.FollowTail {
		grace := s.sessionGrace
		if grace <= 0 {
			grace = 45 * time.Second
		}
		sess := &viewSession{
			id:       newSessionID(),
			hostName: cl.hostName,
			msg:      msg,
			follow:   true,
			grace:    grace,
			client:   cl,
			reg:      s.sessions,
		}
		cl.session = sess
		s.sessions.put(sess)
		slog.Info("启动 follow 会话", "session", sess.id, "host", cl.hostName,
			"file", baseName(msg.FilePath), "grace", grace.String())
		sess.start()
		return
	}
	s.startStatic(cl, msg)
}

// attachSession 重连后尝试接管一个仍存活的 follow 会话，按 lastSeq 补发缺口日志。
// 若会话已失效（宽限到期/进程退出/被销毁），回退到全量 start。
func (s *Server) attachSession(cl *wsClient, msg *wsMessage) {
	s.detachClient(cl, true)
	if msg.SessionID != "" {
		if sess := s.sessions.get(msg.SessionID); sess != nil {
			if sess.attach(cl, msg.LastSeq) {
				cl.session = sess
				return
			}
			// 会话已死：回退到全量启动（下方）。
		}
	}
	// 无可接管会话：若带了 filePath/config 就全新启动，否则什么也不做。
	if msg.FilePath != "" {
		s.startSession(cl, msg)
	}
}

// startStatic 启动一次性读取（非 follow），进程直接绑定到当前连接。
func (s *Server) startStatic(cl *wsClient, msg *wsMessage) {
	if err := msg.Config.Validate(); err != nil {
		s.sendError(cl, "参数错误: "+err.Error())
		return
	}
	h := cl.host
	abs, err := h.ResolvePath(msg.FilePath)
	if err != nil {
		s.sendError(cl, err.Error())
		return
	}
	info, statErr := h.Stat(abs)
	if statErr != nil {
		s.sendError(cl, "无法访问文件: "+statErr.Error())
		return
	}
	if info != nil && info.IsDir() {
		s.sendError(cl, "所选为目录，请选择日志文件")
		return
	}
	var procID uint64
	procID, err = s.runView(h, abs, msg,
		func(batch string) {
			metrics.IncLogBytes(len(batch))
			s.sendText(cl, `{"type":"log","data":`+jsonQuote(batch)+`}`)
		},
		func(errLine string) {
			s.forwardStderr(cl, errLine)
		},
		func() {
			cl.runMu.Lock()
			isCurrent := cl.procID == procID
			if isCurrent {
				cl.procID = 0
			}
			cl.runMu.Unlock()
			s.refreshProcMetric()
			if isCurrent {
				s.sendStatus(cl, "stopped")
			}
		})
	if err != nil {
		s.sendError(cl, viewErrMsg(err))
		return
	}
	cl.runMu.Lock()
	cl.procID = procID
	cl.runMu.Unlock()
	s.refreshProcMetric()
	s.sendStatus(cl, "running")
}

// runView 构建日志查看命令并在给定主机上启动，把 stdout/stderr/退出路由到回调。
// 返回托管后的进程 ID。命令完全由原生 tail/cat/grep/awk/iconv 或 PowerShell 承担，
// Go 不重写其能力（架构原则：一切都是命令）。
func (s *Server) runView(h host.Host, abs string, msg *wsMessage,
	outFn func(string), errFn func(string), doneFn func()) (uint64, error) {
	mode := "static"
	if msg.Config.FollowTail {
		mode = "follow"
	}
	rule := msg.Config.FilterRule
	ts, te, _ := cmdbuild.TimeBounds(rule)
	f := cmdbuild.FilterCfg{
		Pattern:       cmdbuild.AssemblePattern(rule, msg.Config.UseRegex),
		Exclude:       rule.Exclude,
		TimeStart:     ts,
		TimeEnd:       te,
		UseRegex:      msg.Config.UseRegex,
		CaseSensitive: msg.Config.CaseSensitive,
		InvertMatch:   msg.Config.InvertMatch,
		ContextBefore: msg.Config.ContextBefore,
		ContextAfter:  msg.Config.ContextAfter,
	}
	if msg.Config.UseRegex && rule.CustomRegex != "" {
		f.TimeStart, f.TimeEnd = "", ""
	}
	if msgErr := checkCaps(h, msg.Config.Encoding, f.TimeStart != "" || f.TimeEnd != ""); msgErr != "" {
		return 0, errView(msgErr)
	}
	if msgErr := validateFilter(h, f); msgErr != "" {
		return 0, errView(msgErr)
	}
	viewCmd := cmdbuild.BuildView(h.Platform(), mode, abs, msg.Config.Encoding, msg.Config.ReadLinesLimit, f)
	slog.Info("启动日志查看",
		"host", h.Name(), "mode", mode, "file", baseName(abs),
		"config", msg.Config.ConfigName, "shell", viewCmd.Shell)
	proc, err := h.Run(viewCmd)
	if err != nil {
		return 0, err
	}
	return s.procs.Start(proc, outFn, errFn, doneFn)
}

// errView 把 runView 的预检失败（缺命令/非法正则等）包装成普通 error。
// 这类错误文案已是可直接展示的中文说明，前端应原样透传，不加"启动命令失败"前缀。
type errView string

func (e errView) Error() string { return string(e) }

func viewErrMsg(err error) string {
	if _, ok := err.(errView); ok {
		return err.Error()
	}
	return "启动命令失败: " + err.Error()
}

// stopSession 停止当前连接绑定的命令：follow 会话立即销毁（不进宽限），静态进程被杀。
func (s *Server) stopSession(cl *wsClient) {
	s.detachClient(cl, true)
	s.refreshProcMetric()
}

// detachClient 解绑连接与会话/进程。
//   - 若附着的是 follow 会话：killSession=true 时立即销毁（用户 stop/切换/关闭），
//     false 时仅分离连接、会话进入断线宽限（WS 断开时使用）。
//   - 若有静态进程：总是杀掉。
func (s *Server) detachClient(cl *wsClient, killSession bool) {
	cl.runMu.Lock()
	sess := cl.session
	cl.session = nil
	procID := cl.procID
	cl.procID = 0
	cl.runMu.Unlock()

	if procID != 0 {
		s.procs.Stop(procID)
	}
	if sess != nil {
		if killSession {
			sess.destroy()
		} else {
			sess.detach()
		}
	}
}

// ---- 发送辅助（带写锁） ----

// sendText 向客户端写入一条文本消息，带写超时。
//
// 背景：gorilla/websocket 的写操作默认无超时。若客户端半开/网络卡死，WriteMessage
// 会永久阻塞，进而长期持有 wmu，导致日志输出 goroutine 全部卡住、进程数据无法排空。
// 这里设置 wsWriteWait 写截止时间；一旦写入失败（超时或连接已断），关闭底层连接，
// 读循环随即返回并触发 defer 清理（停止进程、关闭连接），从根上解除阻塞。
func (s *Server) sendText(cl *wsClient, text string) {
	cl.wmu.Lock()
	defer cl.wmu.Unlock()
	_ = cl.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if err := cl.conn.WriteMessage(websocket.TextMessage, []byte(text)); err != nil {
		// 写入失败：关闭连接以解除读循环阻塞并回收进程。
		_ = cl.conn.Close()
	}
}

func (s *Server) sendError(cl *wsClient, msg string) {
	s.sendText(cl, `{"type":"error","msg":`+jsonQuote(msg)+`}`)
}

// sendNotice 下发一条非错误通知（日志轮转/截断/断线缺口），由前端显示为可关闭提示条。
func (s *Server) sendNotice(cl *wsClient, kind string) {
	s.sendText(cl, `{"type":"notice","kind":`+jsonQuote(kind)+`}`)
}

// forwardStderr 把一行子进程 stderr 按分类转发为 error 或 notice。
func (s *Server) forwardStderr(cl *wsClient, line string) {
	switch classifyStderr(line) {
	case stderrError:
		s.sendError(cl, strings.TrimSpace(trimLine(line)))
	case stderrRotate:
		s.sendNotice(cl, "rotate")
	case stderrTruncate:
		s.sendNotice(cl, "truncate")
	}
}

func (s *Server) sendStatus(cl *wsClient, status string) {
	s.sendText(cl, `{"type":"status","status":`+jsonQuote(status)+`}`)
}

// jsonQuote 对字符串做 JSON 转义
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

