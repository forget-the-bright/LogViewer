package server

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"logviewer/internal/appconfig"
	"logviewer/internal/cmdbuild"
	"logviewer/internal/host"
	"logviewer/internal/metrics"
	"logviewer/internal/procmgr"
)

// Options 是构造 Server 的参数集合。
type Options struct {
	Hosts      *host.Manager
	Static     fs.FS
	Auth       appconfig.AuthConfig
	ConfigPath string
	// SessionGrace 是 follow 模式下 WS 断线后会话保留的宽限时长（断线补齐）。
	SessionGrace time.Duration
	// ReloadFunc 热加载配置：返回新配置、本次被替换/移除的主机别名列表、错误。
	// 返回的 changed 别名用于通知这些主机上的活跃 WS 连接重连到新实例。
	ReloadFunc func() (cfg *appconfig.AppConfig, changedHosts []string, err error)
	// LogCommandsFunc 返回是否应把每条查询命令打印到服务端日志（开发调试用）。
	// 设计为函数而非固定布尔值，以便配置热加载后即时生效，无需重启。
	LogCommandsFunc func() bool
}

// Server 聚合后端各模块。文件/命令操作全部通过 Host 抽象完成，
// 本机与远程机器共用同一套 HTTP/WS 逻辑。
type Server struct {
	hosts      *host.Manager
	procs      *procmgr.Manager
	static     fs.FS
	auth       *authService
	configPath string
	// reloadFn 热加载配置，返回新配置、被替换/移除的主机别名列表、错误。
	reloadFn func() (cfg *appconfig.AppConfig, changedHosts []string, err error)

	// wsMu 保护 wsClients：记录每个连接绑定的 host 别名，用于热加载替换主机时
	// 通知对应连接重连（拿到新 Host 实例）。
	wsMu      sync.Mutex
	wsClients map[*wsClient]string

	// sessions 管理 follow 会话与 WS 连接的解耦（断线宽限 + 环形缓冲补齐）。
	sessions *sessionRegistry
	// sessionGrace 断线后会话保留宽限。
	sessionGrace time.Duration
	// logCommandsFn 报告是否打印每条查询命令（开发模式）。为 nil 时视为关闭。
	logCommandsFn func() bool
}

// logCmd 在开启 log_commands 时把一条待执行命令打印到服务端日志（DEBUG 级别）。
// kind 标识命令用途（view/export/regex），host 为目标机器名。
func (s *Server) logCmd(kind, host string, cmd cmdbuild.Command) {
	if s.logCommandsFn == nil || !s.logCommandsFn() {
		return
	}
	// 用 INFO 级别（而非 DEBUG）：开发模式开启 log_commands 时，无需同时把
	// log_level 调到 debug 即可看到命令。多行脚本作为单个字段输出，便于复制。
	slog.Info("执行查询命令", "kind", kind, "host", host, "shell", cmd.Shell,
		"platform", cmd.Platform, "script", cmd.Script)
}

// New 创建 Server。
func New(opts Options) *Server {
	srv := &Server{
		hosts:        opts.Hosts,
		procs:        procmgr.NewManager(),
		static:       opts.Static,
		auth:         newAuthService(opts.Auth),
		configPath:   opts.ConfigPath,
		reloadFn:     opts.ReloadFunc,
		wsClients:    map[*wsClient]string{},
		sessionGrace: opts.SessionGrace,
		logCommandsFn: opts.LogCommandsFunc,
	}
	srv.sessions = newSessionRegistry(srv)
	return srv
}

// registerClient 记录一个 WS 连接与其绑定的主机别名。
func (s *Server) registerClient(cl *wsClient, hostName string) {
	s.wsMu.Lock()
	s.wsClients[cl] = hostName
	s.wsMu.Unlock()
}

// unregisterClient 移除连接记录。
func (s *Server) unregisterClient(cl *wsClient) {
	s.wsMu.Lock()
	delete(s.wsClients, cl)
	s.wsMu.Unlock()
}

// NotifyHostsChanged 通知所有绑定到给定主机别名的 WS 连接：主机配置已热更，
// 需重连以绑定到新的 Host 实例（旧实例随后会被 Close 回收）。
// 返回被通知的连接总数。changed 为 Rebuild 返回的被替换/移除主机别名。
func (s *Server) NotifyHostsChanged(changed []string) int {
	if len(changed) == 0 {
		return 0
	}
	// 先收集每个主机名下的连接（持锁时间尽量短），再在锁外发送/关闭。
	s.wsMu.Lock()
	byName := map[string][]*wsClient{}
	for cl, name := range s.wsClients {
		byName[name] = append(byName[name], cl)
	}
	s.wsMu.Unlock()

	n := 0
	for _, hostName := range changed {
		for _, cl := range byName[hostName] {
			// 下发 reconnect 指令：前端收到后重连，/ws?host= 会拿到新实例。
			// 旧进程随连接关闭被 stopSession 回收，避免继续持有即将被 Close 的旧 Host。
			s.sendText(cl, `{"type":"reconnect","reason":"host_reloaded"}`)
			_ = cl.conn.Close()
			n++
		}
	}
	return n
}

// Auth 暴露认证服务（供 main 打印启用状态）。
func (s *Server) Auth() *authService { return s.auth }

// refreshProcMetric 把当前正在运行的日志进程数同步到 Prometheus 指标。
// 在进程启动成功、进程退出、停止会话等状态变化点调用。
func (s *Server) refreshProcMetric() { metrics.SetProcesses(s.procs.Count()) }

// UpdateAuth 热更新认证配置（reload 后调用）。若认证开关状态或用户名变化，清空所有现有会话。
func (s *Server) UpdateAuth(cfg appconfig.AuthConfig) {
	s.auth.mu.Lock()
	authChanged := s.auth.enabled != (cfg.Enabled && cfg.Username != "") || s.auth.username != cfg.Username
	s.auth.enabled = cfg.Enabled && cfg.Username != ""
	s.auth.username = cfg.Username
	s.auth.ttl = time.Duration(cfg.SessionTTLMinutes) * time.Minute
	s.auth.check = cfg.ValidatePassword
	if authChanged {
		s.auth.sessions = map[string]time.Time{}
	}
	s.auth.mu.Unlock()
}

// Close 释放后端资源：先销毁所有 follow 会话（连带进程），再关闭所有机器连接
// （SSH keepalive/客户端）。供优雅关闭时调用。
func (s *Server) Close() {
	s.sessions.mu.Lock()
	sessions := make([]*viewSession, 0, len(s.sessions.m))
	for _, sess := range s.sessions.m {
		sessions = append(sessions, sess)
	}
	s.sessions.mu.Unlock()
	for _, sess := range sessions {
		sess.destroy()
	}
	s.procs.StopAll()
	s.hosts.Close()
}

// hostFrom 请求中提取 :host 参数并返回对应的 Host。
func (s *Server) hostFrom(c *gin.Context) (host.Host, bool) {
	name := c.Param("host")
	h, err := s.hosts.Get(name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return nil, false
	}
	return h, true
}

// checkCaps 校验目标机器是否具备执行该配置所需的原生命令能力。
// 返回空串表示 OK，否则返回可读的错误说明。本机恒通过。
func checkCaps(h host.Host, encoding string, hasTimeFilter bool) string {
	caps := h.Capabilities()
	if cmdbuild.IsGBK(encoding) && !caps.HasIconv {
		return "远端机器缺少 iconv，无法进行 GBK 转码，请在远端安装后重试"
	}
	if hasTimeFilter && !caps.HasAwk {
		return "远端机器缺少 awk，无法按时间范围过滤，请在远端安装后重试"
	}
	if !caps.HasTail || !caps.HasCat || !caps.HasGrep {
		return "远端机器缺少必要命令（tail/cat/grep），无法查看日志"
	}
	return ""
}

// upgrader WebSocket 升级器。CheckOrigin 在启用认证时做同源校验，防止跨站 WS 劫持。
func (s *Server) upgrader() websocket.Upgrader {
	return websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			if !s.auth.Enabled() {
				return true
			}
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // 非浏览器客户端
			}
			// 只比较 host 部分（本地可能 http/ws 混合，不强制 scheme）。
			return sameOriginHost(origin, r.Host)
		},
	}
}

// sameOriginHost 判断 Origin 的 host 部分与请求 Host 是否一致（忽略端口差异的反向情形按同源严格比较）。
func sameOriginHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// Router 组装全部路由
func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	api := r.Group("/api")
	// 登录/状态接口本身不鉴权
	api.POST("/login", s.handleLogin)
	api.GET("/auth/status", s.handleAuthStatus)
	// 其余 /api/** 全部需要登录（未启用认证时中间件直接放行）
	authApi := api.Group("/")
	authApi.Use(s.authRequired())
	{
		authApi.POST("/logout", s.handleLogout)
		authApi.POST("/reload", s.handleReload)
		authApi.GET("/hosts", s.handleHosts)

		h := authApi.Group("/h/:host")
		{
			h.GET("/capabilities", s.handleCapabilities)
			h.GET("/dir/roots", s.handleListRoots)
			h.GET("/dir/list", s.handleListDir)

			h.GET("/config/list", s.handleConfigList)
			h.GET("/config/get", s.handleConfigGet)
			h.POST("/config/save", s.handleConfigSave)
			h.POST("/config/delete", s.handleConfigDelete)
			h.POST("/config/rename", s.handleConfigRename)
			h.POST("/config/setdefault", s.handleConfigSetDefault)
			h.POST("/config/preview", s.handleConfigPreview)

			h.GET("/file/download/origin", s.handleDownloadOrigin)
			h.POST("/file/download/filter", s.handleDownloadFilter)
		}
	}

	r.GET("/ws", s.handleWS)

	// 可观测性端点：免鉴权（不含日志内容/敏感数据），便于监控系统直接抓取。
	// /healthz 返回各主机连通状态；/metrics 暴露 Prometheus 指标。
	r.GET("/healthz", s.handleHealthz)
	r.GET("/metrics", gin.WrapH(metrics.Handler()))

	// 静态资源（embed 的单文件前端）。
	// 本工具每次发布都可能改 app.js/style.css，强制浏览器每次 revalidate，
	// 避免用户拿到旧缓存导致"明明修了却还复现"的假象。
	if s.static != nil {
		serveNoCache := func(h http.Handler) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
				h.ServeHTTP(w, req)
			}
		}
		r.GET("/", gin.WrapF(serveNoCache(http.FileServer(http.FS(s.static)))))
		r.GET("/static/*filepath", gin.WrapF(serveNoCache(http.StripPrefix("/static", http.FileServer(http.FS(s.static))))))
	}
	return r
}

// handleHosts 返回所有机器概要，供顶栏切换器使用。
func (s *Server) handleHosts(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"hosts": s.hosts.List()})
}

// handleReload 触发配置热加载（重新读取配置文件、重建主机集合）。
func (s *Server) handleReload(c *gin.Context) {
	if s.reloadFn == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "服务器未配置热加载功能"})
		return
	}
	newCfg, changed, err := s.reloadFn()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "配置重载失败: " + err.Error()})
		return
	}
	s.UpdateAuth(newCfg.Auth)
	// 配置变更的主机：通知其活跃 WS 连接重连到新实例（在响应返回后触发）。
	go s.NotifyHostsChanged(changed)
	c.JSON(http.StatusOK, gin.H{
		"ok":    true,
		"hosts": s.hosts.List(),
	})
}

// handleCapabilities 返回目标机器的原生命令能力（前端据此禁用 GBK/时间过滤等）。
func (s *Server) handleCapabilities(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.Capabilities())
}

// hostHealth 是 /healthz 返回中单台主机的健康视图。
type hostHealth struct {
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Online    bool   `json:"online"`
	Available bool   `json:"available"`
	Message   string `json:"message,omitempty"`
}

// handleHealthz 返回所有主机的连通状态，供外部监控/负载均衡接入。
// 对每台主机执行一次（被节流的）轻量探活：任一 SSH 主机不可用则整体 503 degraded，
// 本机恒为 ok。免鉴权（不含敏感数据）。
func (s *Server) handleHealthz(c *gin.Context) {
	infos := s.hosts.List()
	hosts := make([]hostHealth, 0, len(infos))
	allOK := true
	for _, info := range infos {
		h, err := s.hosts.Get(info.Name)
		if err != nil {
			allOK = false
			hosts = append(hosts, hostHealth{Name: info.Name, Message: err.Error()})
			continue
		}
		// 触发一次节流后的探活，刷新在线状态。
		checkErr := h.HealthCheck()
		// 重新取 Info()，以便反映探活后的 online/lastErr。
		fresh := h.Info()
		available := checkErr == nil
		if !available {
			allOK = false
		}
		msg := fresh.Message
		if checkErr != nil && msg == "" {
			msg = checkErr.Error()
		}
		hosts = append(hosts, hostHealth{
			Name:      info.Name,
			Platform:  fresh.Platform,
			Online:    fresh.Online,
			Available: available,
			Message:   msg,
		})
	}
	status := http.StatusOK
	overall := "ok"
	if !allOK {
		status = http.StatusServiceUnavailable
		overall = "degraded"
	}
	c.JSON(status, gin.H{"status": overall, "hosts": hosts})
}
