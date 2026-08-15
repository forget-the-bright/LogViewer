package server

import (
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"logviewer/internal/appconfig"
	"logviewer/internal/host"
	"logviewer/internal/procmgr"
)

// Options 是构造 Server 的参数集合。
type Options struct {
	Hosts      *host.Manager
	Static     fs.FS
	Auth       appconfig.AuthConfig
	ConfigPath string
	ReloadFunc func() (*appconfig.AppConfig, error)
}

// Server 聚合后端各模块。文件/命令操作全部通过 Host 抽象完成，
// 本机与远程机器共用同一套 HTTP/WS 逻辑。
type Server struct {
	hosts      *host.Manager
	procs      *procmgr.Manager
	static     fs.FS
	auth       *authService
	configPath string
	reloadFn   func() (*appconfig.AppConfig, error)
}

// New 创建 Server。
func New(opts Options) *Server {
	return &Server{
		hosts:      opts.Hosts,
		procs:      procmgr.NewManager(),
		static:     opts.Static,
		auth:       newAuthService(opts.Auth),
		configPath: opts.ConfigPath,
		reloadFn:   opts.ReloadFunc,
	}
}

// Auth 暴露认证服务（供 main 打印启用状态）。
func (s *Server) Auth() *authService { return s.auth }

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

// Close 释放后端资源：先停掉所有正在运行的日志进程（含远程进程组查杀），
// 再关闭所有机器连接（SSH keepalive/客户端）。供优雅关闭时调用。
func (s *Server) Close() {
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
	if isGBK(encoding) && !caps.HasIconv {
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

func isGBK(enc string) bool {
	enc = strings.ToLower(strings.TrimSpace(enc))
	return enc == "gbk" || enc == "gb2312"
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
	newCfg, err := s.reloadFn()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "配置重载失败: " + err.Error()})
		return
	}
	s.UpdateAuth(newCfg.Auth)
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
