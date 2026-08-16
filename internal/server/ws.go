package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
	"logviewer/internal/host"
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

	runMu     sync.Mutex
	procID    uint64
	runGen    uint64
	runCancel context.CancelFunc
}

// filePollInterval 等待暂不存在的日志文件时的轮询间隔。
const filePollInterval = 500 * time.Millisecond

// wsMessage 上行指令
type wsMessage struct {
	Action   string           `json:"action"`
	FilePath string           `json:"filePath"`
	Config   config.LogConfig `json:"config"`
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
		s.stopSession(cl)
		s.unregisterClient(cl)
		conn.Close()
	}()

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
		case "stop":
			s.stopSession(cl)
			s.sendStatus(cl, "stopped")
		case "ping":
			s.sendText(cl, `{"type":"status","status":"alive"}`)
		}
	}
}

// startSession 启动一次日志查看：构建系统原生命令管道并托管其进程。
//
// 跟踪模式下文件允许暂不存在：此时不立即启动命令（旧实现会立即 tail 一个不存在的
// 文件，瞬间把 "No such file" 抛给前端、状态在 waiting/running/error 间闪烁），
// 而是先下发 waiting，在后台轮询直到文件出现（或被 stop/切换取消）再启动。
func (s *Server) startSession(cl *wsClient, msg *wsMessage) {
	// 开新一代任务：取消上一代的等待/进程。
	cl.runMu.Lock()
	if cl.runCancel != nil {
		cl.runCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cl.runGen++
	gen := cl.runGen
	cl.runCancel = cancel
	if cl.procID != 0 {
		s.procs.Stop(cl.procID)
		cl.procID = 0
	}
	cl.runMu.Unlock()

	// 在拼命令前拦截越界的数值参数（读取行数/上下文行数），避免构造出
	// tail -n 1000000000 之类的病态命令行。
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

	// 同步做目录/存在性预检：
	//   - 目录、明确的权限错误等：立即报错，不进入等待。
	//   - follow 且文件不存在：进入后台等待。
	info, statErr := h.Stat(abs)
	if statErr != nil {
		if os.IsNotExist(statErr) && msg.Config.FollowTail {
			s.sendStatus(cl, "waiting")
			go s.waitForFileAndRun(cl, gen, ctx, abs, msg)
			return
		}
		s.sendError(cl, "无法访问文件: "+statErr.Error())
		return
	}
	if info != nil && info.IsDir() {
		s.sendError(cl, "所选为目录，请选择日志文件")
		return
	}

	s.launchView(cl, gen, abs, msg)
}

// waitForFileAndRun 在后台轮询直到目标文件出现（或被取消），然后启动查看。
// gen 用于在启动前确认自己仍是当前这一代任务。
func (s *Server) waitForFileAndRun(cl *wsClient, gen uint64, ctx context.Context, abs string, msg *wsMessage) {
	t := time.NewTicker(filePollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		info, err := cl.host.Stat(abs)
		if err == nil && info != nil && !info.IsDir() {
			// 文件出现。若期间已被新一代任务取代，则放弃启动。
			cl.runMu.Lock()
			current := cl.runGen == gen
			cl.runMu.Unlock()
			if current {
				s.launchView(cl, gen, abs, msg)
			}
			return
		}
	}
}

// launchView 构建命令并启动进程。gen 用于过期回调保护：进程结束时只有仍是当前代
// 才下发 stopped 状态，避免旧代的进程退出把新一代任务误报成已停止。
func (s *Server) launchView(cl *wsClient, gen uint64, abs string, msg *wsMessage) {
	h := cl.host
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
	// 自定义正则仅在勾选"正则"时生效，并覆盖时间阶段
	if msg.Config.UseRegex && rule.CustomRegex != "" {
		f.TimeStart, f.TimeEnd = "", ""
	}
	if msgErr := checkCaps(h, msg.Config.Encoding, f.TimeStart != "" || f.TimeEnd != ""); msgErr != "" {
		s.sendError(cl, msgErr)
		return
	}
	// 用目标机器的原生引擎预校验正则：非法时立即返回可读错误，
	// 而不是启动管道后再从 stderr 冒出生硬的引擎报错。
	if msgErr := validateFilter(h, f); msgErr != "" {
		s.sendError(cl, msgErr)
		return
	}
	viewCmd := cmdbuild.BuildView(h.Platform(), mode, abs, msg.Config.Encoding, msg.Config.ReadLinesLimit, f)
	log.Printf("[ws] host=%s mode=%s file=%s shell=%s", h.Name(), mode, baseName(abs), viewCmd.Shell)
	proc, err := h.Run(viewCmd)
	if err != nil {
		s.sendError(cl, "启动命令失败: "+err.Error())
		return
	}

	var procID uint64
	procID, err = s.procs.Start(proc, func(batch string) {
		s.sendText(cl, `{"type":"log","data":`+jsonQuote(batch)+`}`)
	}, func(errLine string) {
		if line := classifyStderr(errLine); line != "" {
			s.sendError(cl, line)
		}
	}, func() {
		// 进程自然退出：仅当它仍是当前代任务时才通知前端 stopped。
		cl.runMu.Lock()
		isCurrent := cl.procID == procID && cl.runGen == gen
		if isCurrent {
			cl.procID = 0
		}
		cl.runMu.Unlock()
		if isCurrent {
			s.sendStatus(cl, "stopped")
		}
	})
	if err != nil {
		s.sendError(cl, "启动命令失败: "+err.Error())
		return
	}
	cl.runMu.Lock()
	if cl.runGen != gen {
		// 启动期间已被新一代取代：立即停掉这个进程，不更新状态。
		cl.runMu.Unlock()
		s.procs.Stop(procID)
		return
	}
	cl.procID = procID
	cl.runMu.Unlock()
	s.sendStatus(cl, "running")
}

// stopSession 停止当前连接绑定的命令进程与等待中的文件轮询。
func (s *Server) stopSession(cl *wsClient) {
	cl.runMu.Lock()
	if cl.runCancel != nil {
		cl.runCancel()
	}
	procID := cl.procID
	cl.procID = 0
	cl.runMu.Unlock()
	if procID != 0 {
		s.procs.Stop(procID)
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

