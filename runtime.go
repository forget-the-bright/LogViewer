package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"logviewer/internal/appconfig"
	"logviewer/internal/applog"
	"logviewer/internal/cmdbuild"
	"logviewer/internal/host"
	"logviewer/internal/server"

	"github.com/gin-gonic/gin"
)

// service 聚合一次进程启动后构造出的后端运行时：主机管理器、Gin server、
// 当前配置及配置重载/日志开关闭包。web 与 gui 两种模式共用同一套装配，
// 仅在最外层的"如何监听 / 如何展示窗口"上分叉。
type service struct {
	srv       *server.Server
	hm        *host.Manager
	appCfg    *appconfig.AppConfig
	cfgPath   string
	extraDirs []string
	key       string
	logOutput io.Writer

	cfgMu sync.Mutex
}

// buildService 加载配置、初始化日志、构造全部主机与 Gin server。
// logOutput 为 nil 时日志写 os.Stderr；GUI 模式传入日志文件。
func buildService(staticFS fs.FS, cfgPath string, extraDirs []string, key string, logOutput io.Writer) (*service, error) {
	if logOutput == nil {
		logOutput = os.Stderr
	}

	appCfg, loadedCfgPath, err := appconfig.Load(cfgPath, extraDirs)
	if err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}
	applog.InitWithOutput(appCfg.LogJSON, appCfg.LogLevel, logOutput)
	slog.Info("配置文件已加载", "path", loadedCfgPath, "log_json", appCfg.LogJSON, "log_level", appCfg.LogLevel)
	if !appCfg.GIN_MODE_DEBUG {
		gin.SetMode(gin.ReleaseMode)
	}
	if appCfg.HasEncryptedPasswords() {
		if key == "" {
			return nil, errors.New("配置中包含加密密码，必须通过 -key 或 LOGVIEWER_KEY 环境变量提供解密密钥")
		}
		if err := appCfg.DecryptPasswords(key); err != nil {
			return nil, fmt.Errorf("解密配置密码失败: %w", err)
		}
		slog.Info("已使用密钥在内存中解密配置密码")
	}

	hm, err := buildHostManager(appCfg, loadedCfgPath)
	if err != nil {
		return nil, fmt.Errorf("初始化机器失败: %w", err)
	}
	logRegisteredHosts(hm, appCfg)

	s := &service{
		hm:        hm,
		appCfg:    appCfg,
		cfgPath:   loadedCfgPath,
		extraDirs: extraDirs,
		key:       key,
		logOutput: logOutput,
	}
	s.srv = server.New(server.Options{
		Hosts:           hm,
		Static:          staticFS,
		Auth:            appCfg.Auth,
		ConfigPath:      loadedCfgPath,
		SessionGrace:    time.Duration(appCfg.SessionGraceSeconds) * time.Second,
		ReloadFunc:      s.Reload,
		LogCommandsFunc: s.logCommands,
	})
	return s, nil
}

// Router 返回底层 Gin 引擎，供 web 模式挂到 http.Server、gui 模式挂到本地 listener。
func (s *service) Router() *gin.Engine { return s.srv.Router() }

// Addr 返回配置中的监听地址（web 模式用；gui 模式固定 127.0.0.1:0 忽略它）。
func (s *service) Addr() string { return s.appCfg.Addr }

// Reload 重新读取配置并重建主机集合，返回新配置与被替换/移除的主机别名。
// 持锁更新 appCfg；调用方（HTTP /api/reload 或 SIGHUP）负责随后 UpdateAuth/通知 WS。
func (s *service) Reload() (*appconfig.AppConfig, []string, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	newCfg, _, err := appconfig.Load(s.cfgPath, s.extraDirs)
	if err != nil {
		return nil, nil, err
	}
	if newCfg.HasEncryptedPasswords() {
		if s.key == "" {
			return nil, nil, errors.New("配置中包含加密密码，但未提供解密密钥")
		}
		if err := newCfg.DecryptPasswords(s.key); err != nil {
			return nil, nil, err
		}
	}
	changed, err := rebuildHostManager(s.hm, newCfg, s.cfgPath)
	if err != nil {
		return nil, nil, err
	}
	s.appCfg = newCfg
	return newCfg, changed, nil
}

// ReinitLogging 用当前配置重新初始化日志输出（reload 后日志配置可能变化）。
func (s *service) ReinitLogging() {
	s.cfgMu.Lock()
	cfg := s.appCfg
	s.cfgMu.Unlock()
	applog.InitWithOutput(cfg.LogJSON, cfg.LogLevel, s.logOutput)
}

// UpdateAuth 热更新认证配置。
func (s *service) UpdateAuth(cfg appconfig.AuthConfig) { s.srv.UpdateAuth(cfg) }

// NotifyHostsChanged 通知绑定到变更主机的 WS 连接重连，返回连接数。
func (s *service) NotifyHostsChanged(changed []string) int { return s.srv.NotifyHostsChanged(changed) }

// Close 释放后端资源：销毁 follow 会话、停止所有日志进程、关闭 SSH 连接。
func (s *service) Close() { s.srv.Close() }

// logCommands 报告是否应把每条查询命令打印到日志（与 Reload 互斥）。
func (s *service) logCommands() bool {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.appCfg.LogCommands
}

// logRegisteredHosts 打印主机注册结果与本机 PowerShell 选型。
func logRegisteredHosts(hm *host.Manager, appCfg *appconfig.AppConfig) {
	for _, info := range hm.List() {
		kind := "local"
		if !info.Local {
			kind = "ssh"
		}
		platform := info.Platform
		if platform == "" {
			platform = "probing"
		}
		exts := appCfg.Hosts[info.Name].FileExtensions
		extStr := strings.Join(exts, ",")
		if extStr == "" {
			extStr = ".log,.out（默认）"
		}
		display := info.DisplayName
		if display == "" {
			display = info.Name
		}
		slog.Info("主机已注册", "kind", kind, "name", info.Name, "display_name", display,
			"platform", platform, "dirs", dirsOf(hm, info.Name), "file_extensions", extStr)
	}
	if local, _ := hm.Get("local"); local != nil && local.Platform() == "windows" {
		ps := cmdbuild.LocalPowerShell()
		if strings.EqualFold(ps, "powershell") {
			slog.Info("本机 PowerShell：powershell 5.1（未检测到 pwsh 7+，安装后日志操作启动可快约 5 倍）", "exe", ps)
		} else {
			slog.Info("本机 PowerShell：pwsh 7+", "exe", ps)
		}
	}
}
