package server

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
)

// wsClient 每个连接的状态：写锁 + 当前子进程
type wsClient struct {
	conn   *websocket.Conn
	wmu    sync.Mutex
	procID uint64
}

// 连接 -> 客户端状态
var (
	clientsMu sync.Mutex
	clients   = map[*websocket.Conn]*wsClient{}
)

// wsMessage 上行指令
type wsMessage struct {
	Action   string           `json:"action"`
	FilePath string           `json:"filePath"`
	Config   config.LogConfig `json:"config"`
}

// handleWS 处理 WebSocket 连接
func (s *Server) handleWS(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	cl := &wsClient{conn: conn}
	clientsMu.Lock()
	clients[conn] = cl
	clientsMu.Unlock()

	// 连接断开：销毁该连接绑定的命令进程，防止残留
	defer func() {
		s.stopSession(cl)
		clientsMu.Lock()
		delete(clients, conn)
		clientsMu.Unlock()
		conn.Close()
	}()

	conn.SetReadLimit(1 << 20) // 1MB
	conn.SetReadDeadline(time.Time{})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
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
// 静态加载与实时跟踪都走命令，Go 只做转发（纯外壳）。
func (s *Server) startSession(cl *wsClient, msg *wsMessage) {
	// 先停掉旧进程
	s.stopSession(cl)

	abs, err := s.resolveAndCheck(msg.FilePath)
	if err != nil {
		s.sendError(cl, err.Error())
		return
	}

	// 文件预检
	info, err := os.Stat(abs)
	missing := os.IsNotExist(err)
	if missing && msg.Config.FollowTail {
		// 跟踪模式允许文件暂不存在（tail -F / Get-Content -Wait 会等待）
		s.sendStatus(cl, "waiting")
	} else if err != nil {
		s.sendError(cl, "无法访问文件: "+err.Error())
		return
	} else if info != nil && info.IsDir() {
		s.sendError(cl, "所选为目录，请选择日志文件")
		return
	}

	mode := "static"
	if msg.Config.FollowTail {
		mode = "follow"
	}
	rule := msg.Config.FilterRule
	ts, te, _ := cmdbuild.TimeBounds(rule)
	f := cmdbuild.FilterCfg{
		Pattern:        cmdbuild.AssemblePattern(rule, msg.Config.UseRegex),
		Exclude:        rule.Exclude,
		TimeStart:      ts,
		TimeEnd:        te,
		UseRegex:       msg.Config.UseRegex,
		CaseSensitive:  msg.Config.CaseSensitive,
		InvertMatch:    msg.Config.InvertMatch,
		ContextBefore:  msg.Config.ContextBefore,
		ContextAfter:   msg.Config.ContextAfter,
	}
	// 自定义正则仅在勾选"正则"时生效，并覆盖时间阶段
	if msg.Config.UseRegex && rule.CustomRegex != "" {
		f.TimeStart, f.TimeEnd = "", ""
	}
	cmd := cmdbuild.BuildView(mode, abs, msg.Config.Encoding, msg.Config.ReadLinesLimit, f).BuildCmd()

	procID, err := s.procs.Start(cmd, func(batch string) {
		s.sendText(cl, `{"type":"log","data":`+jsonQuote(batch)+`}`)
	}, func(errLine string) {
		s.sendError(cl, trimLine(errLine))
	}, func() {
		s.stopSession(cl)
		s.sendStatus(cl, "stopped")
	})
	if err != nil {
		s.sendError(cl, "启动命令失败: "+err.Error())
		return
	}
	cl.procID = procID
	s.sendStatus(cl, "running")
}

// stopSession 停止当前连接绑定的命令进程
func (s *Server) stopSession(cl *wsClient) {
	if cl.procID != 0 {
		s.procs.Stop(cl.procID)
		cl.procID = 0
	}
}

// ---- 发送辅助（带写锁） ----

func (s *Server) sendText(cl *wsClient, text string) {
	cl.wmu.Lock()
	defer cl.wmu.Unlock()
	_ = cl.conn.WriteMessage(websocket.TextMessage, []byte(text))
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