package server

import (
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

// wsClient 每个连接的状态：写锁 + 当前子进程 + 所属机器
type wsClient struct {
	conn   *websocket.Conn
	wmu    sync.Mutex
	host   host.Host
	procID uint64
}

// wsMessage 上行指令
type wsMessage struct {
	Action   string           `json:"action"`
	FilePath string           `json:"filePath"`
	Config   config.LogConfig `json:"config"`
}

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
	h, err := s.hosts.Get(c.Query("host"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	upg := s.upgrader()
	conn, err := upg.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	cl := &wsClient{conn: conn, host: h}

	defer func() {
		s.stopSession(cl)
		conn.Close()
	}()

	conn.SetReadLimit(1 << 20)
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
func (s *Server) startSession(cl *wsClient, msg *wsMessage) {
	s.stopSession(cl)
	h := cl.host

	abs, err := h.ResolvePath(msg.FilePath)
	if err != nil {
		s.sendError(cl, err.Error())
		return
	}

	// 文件预检（跟踪模式允许文件暂不存在）
	info, statErr := h.Stat(abs)
	missing := os.IsNotExist(statErr)
	if missing && msg.Config.FollowTail {
		s.sendStatus(cl, "waiting")
	} else if statErr != nil {
		s.sendError(cl, "无法访问文件: "+statErr.Error())
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
	viewCmd := cmdbuild.BuildView(h.Platform(), mode, abs, msg.Config.Encoding, msg.Config.ReadLinesLimit, f)
	log.Printf("[ws] host=%s mode=%s file=%s shell=%s", h.Name(), mode, baseName(abs), viewCmd.Shell)
	proc, err := h.Run(viewCmd)
	if err != nil {
		s.sendError(cl, "启动命令失败: "+err.Error())
		return
	}

	procID, err := s.procs.Start(proc, func(batch string) {
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

