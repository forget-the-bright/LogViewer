package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"logviewer/internal/appconfig"
	"logviewer/internal/config"
	"logviewer/internal/host"
	"logviewer/internal/server"
)

//go:embed all:static
var staticFS embed.FS

func main() {
	addr := flag.String("addr", "", "HTTP 监听地址（覆盖 logviewer.json 中的 addr）")
	dir := flag.String("dir", "", "允许扫描的根工作目录（逗号/分号分隔，合并到本机 local 主机）")
	configPath := flag.String("config", "", "配置文件路径（默认 <exe>/logviewer.json，其次 <cwd>/logviewer.json）")
	hashPw := flag.String("hash-password", "", "生成 bcrypt 密码哈希后退出（用于配置 auth.password）")
	flag.Parse()

	if *hashPw != "" {
		h, err := appconfig.HashPassword(*hashPw)
		if err != nil {
			log.Fatalf("生成哈希失败: %v", err)
		}
		fmt.Println(h)
		return
	}

	// 静态资源
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("加载静态资源失败: %v", err)
	}

	// 加载配置（首次运行会生成带注释模板，并迁移旧 configs.json）
	var extraDirs []string
	if *dir != "" {
		extraDirs = splitList(*dir)
	}
	appCfg, cfgPath, err := appconfig.Load(*configPath, extraDirs)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	log.Printf("配置文件: %s", cfgPath)

	listenAddr := *addr
	if listenAddr == "" {
		listenAddr = appCfg.Addr
	}

	// 构造 host.Manager。阶段一只有 local 实际可用；配置中的 SSH 主机暂不连接。
	hm, err := buildHostManager(appCfg, cfgPath)
	if err != nil {
		log.Fatalf("初始化机器失败: %v", err)
	}
	for _, info := range hm.List() {
		kind := "本机"
		if !info.Local {
			kind = "SSH"
		}
		platform := info.Platform
		if platform == "" {
			platform = "探测中"
		}
		log.Printf("机器: [%s] %s (%s) dirs=%v", kind, info.Name, platform, dirsOf(hm, info.Name))
	}

	srv := server.New(hm, sub, appCfg.Auth)
	displayAddr := listenAddr
	if strings.HasPrefix(displayAddr, ":") {
		displayAddr = "127.0.0.1" + displayAddr
	}
	logStartupWarnings(appCfg, listenAddr)

	httpSrv := &http.Server{Addr: listenAddr, Handler: srv.Router()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("LogViewer 启动，访问 http://%s", displayAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务启动失败: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("收到退出信号，正在关闭（停止日志进程与远程连接）...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP 关闭超时: %v", err)
	}
	srv.Close()
	log.Printf("已退出")
}

// logStartupWarnings 打印安全相关的启动告警：明文登录密码、非 loopback 无认证。
// authEnabled 是配置层面“认证是否生效”的统一判定：enabled=true 且用户名非空。
func authEnabled(cfg *appconfig.AppConfig) bool {
	return cfg.Auth.Enabled && cfg.Auth.Username != ""
}

func logStartupWarnings(cfg *appconfig.AppConfig, addr string) {
	if authEnabled(cfg) {
		ttl := time.Duration(cfg.Auth.SessionTTLMinutes) * time.Minute
		log.Printf("登录认证已启用（用户: %s，会话有效期: %s）", cfg.Auth.Username, ttl)
		if !appconfig.IsBcryptHash(cfg.Auth.Password) {
			log.Printf("警告: auth.password 为明文，建议用 `logviewer -hash-password <明文>` 生成 bcrypt 哈希后填写")
		}
	} else {
		log.Printf("登录认证未启用（auth.enabled=false 或用户名为空），所有功能可直接访问")
	}
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		h = strings.Trim(h, "[]")
		if h != "127.0.0.1" && h != "localhost" && h != "::1" && !authEnabled(cfg) {
			log.Printf("警告: 监听在非本机地址 %s 但未启用登录认证，存在未授权访问风险", h)
		}
	}
}

// buildHostManager 从 AppConfig 构造所有 Host。
// 每台机器的过滤预设变更时，通过闭包回写到 AppConfig 并持久化到 logviewer.json。
func buildHostManager(appCfg *appconfig.AppConfig, cfgPath string) (*host.Manager, error) {
	var mu sync.Mutex
	saveCfgFor := func(name string) config.SaveFunc {
		return func(store config.ConfigStore) error {
			mu.Lock()
			defer mu.Unlock()
			appCfg.UpdateHostConfigs(name, store)
			return appconfig.Save(cfgPath, appCfg)
		}
	}

	localCfg := appCfg.Hosts["local"]
	local, err := host.NewLocalHost("local", localCfg.Dirs, localCfg.Configs, saveCfgFor("local"))
	if err != nil {
		return nil, err
	}
	hosts := []host.Host{local}

	// 远程主机（保持配置里的顺序，local 之后）。
	for name, hc := range appCfg.Hosts {
		if name == "local" || hc.SSH == nil {
			continue
		}
		sh, err := host.NewSSHHost(name, *hc.SSH, hc.Platform, hc.Dirs, hc.Configs, saveCfgFor(name))
		if err != nil {
			return nil, fmt.Errorf("初始化机器 %q 失败: %w", name, err)
		}
		hosts = append(hosts, sh)
	}
	return host.NewManager(hosts...)
}

func dirsOf(hm *host.Manager, name string) []string {
	h, err := hm.Get(name)
	if err != nil {
		return nil
	}
	return h.Dirs()
}

func splitList(s string) []string {
	var out []string
	for _, p := range splitByAny(s, ',', ';') {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitByAny(s string, seps ...byte) []string {
	var out []string
	var cur []rune
	for _, r := range s {
		matched := false
		for _, sep := range seps {
			if r == rune(sep) {
				matched = true
				break
			}
		}
		if matched {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = nil
			}
			continue
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}
