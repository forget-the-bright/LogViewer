package server

import (
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"logviewer/internal/appconfig"
	"logviewer/internal/host"
	"logviewer/internal/procmgr"
)

// Server 聚合后端各模块。文件/命令操作全部通过 Host 抽象完成，
// 本机与远程机器共用同一套 HTTP/WS 逻辑。
type Server struct {
	hosts  *host.Manager
	procs  *procmgr.Manager
	static fs.FS
	auth   *authService
}

// New 创建 Server。static 为嵌入式前端资源（可为 nil），authCfg 控制登录认证。
func New(hosts *host.Manager, static fs.FS, authCfg appconfig.AuthConfig) *Server {
	return &Server{
		hosts:  hosts,
		procs:  procmgr.NewManager(),
		static: static,
		auth:   newAuthService(authCfg),
	}
}

// Auth 暴露认证服务（供 main 打印启用状态）。
func (s *Server) Auth() *authService { return s.auth }

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

// handleCapabilities 返回目标机器的原生命令能力（前端据此禁用 GBK/时间过滤等）。
func (s *Server) handleCapabilities(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.Capabilities())
}
