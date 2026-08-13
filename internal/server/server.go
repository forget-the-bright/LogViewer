package server

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"logviewer/internal/config"
	"logviewer/internal/procmgr"
)

// Server 聚合后端各模块
type Server struct {
	cfg    *config.Manager
	procs  *procmgr.Manager
	mu     sync.Mutex
	roots  []string // 允许访问的根工作目录（绝对路径）
	static fs.FS    // 嵌入式静态资源
}

// New 创建 Server。roots 为允许的根工作目录（默认会追加当前工作目录）。
func New(configDir string, static fs.FS, roots ...string) (*Server, error) {
	cfg, err := config.NewManager(configDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:    cfg,
		procs:  procmgr.NewManager(),
		static: static,
	}
	// 默认根：当前工作目录
	cwd, err := os.Getwd()
	if err == nil {
		s.addRoot(cwd)
	}
	for _, r := range roots {
		s.addRoot(r)
	}
	// 若配置文件目录也在允许范围内，一并加入（便于浏览配置）
	absCfg, aerr := filepath.Abs(configDir)
	if aerr == nil {
		s.addRoot(absCfg)
	}
	return s, nil
}

// addRoot 规范化并加入根目录（去重）
func (s *Server) addRoot(p string) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return
	}
	abs = filepath.Clean(abs)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.roots {
		if r == abs {
			return
		}
	}
	s.roots = append(s.roots, abs)
}

// rootsSnapshot 返回根目录列表副本
func (s *Server) rootsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.roots))
	copy(out, s.roots)
	return out
}

// resolveAndCheck 校验并规范化路径：必须位于某个允许根目录之内（路径穿越防护）。
func (s *Server) resolveAndCheck(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errPath("路径为空")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", errPath("路径无效: " + err.Error())
	}
	abs = filepath.Clean(abs)
	for _, r := range s.roots {
		rel, err := filepath.Rel(r, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return abs, nil
		}
	}
	return "", errPath("访问超出允许范围: " + p)
}

// pathError 路径安全相关错误
type pathError struct{ msg string }

func (e *pathError) Error() string { return e.msg }

func errPath(m string) error { return &pathError{m} }

// upgrader WebSocket 升级器
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Router 组装全部路由
func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	api := r.Group("/api")
	{
		api.GET("/dir/roots", s.handleListRoots)
		api.GET("/dir/list", s.handleListDir)
		api.GET("/config/list", s.handleConfigList)
		api.GET("/config/get", s.handleConfigGet)
		api.POST("/config/save", s.handleConfigSave)
		api.POST("/config/delete", s.handleConfigDelete)
		api.POST("/config/rename", s.handleConfigRename)
		api.POST("/config/setdefault", s.handleConfigSetDefault)
		api.POST("/config/preview", s.handleConfigPreview)
		api.GET("/file/download/origin", s.handleDownloadOrigin)
		api.POST("/file/download/filter", s.handleDownloadFilter)
	}

	r.GET("/ws", s.handleWS)

	// 静态资源（embed 的单文件前端）
	if s.static != nil {
		r.GET("/", gin.WrapF(func(w http.ResponseWriter, req *http.Request) {
			http.FileServer(http.FS(s.static)).ServeHTTP(w, req)
		}))
		r.GET("/static/*filepath", gin.WrapF(func(w http.ResponseWriter, req *http.Request) {
			http.StripPrefix("/static", http.FileServer(http.FS(s.static))).ServeHTTP(w, req)
		}))
	}
	return r
}