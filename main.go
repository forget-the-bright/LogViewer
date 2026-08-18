package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
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
	"logviewer/internal/cryptoutil"
	"logviewer/internal/host"
	"logviewer/internal/metrics"
)

//go:embed all:static
var staticFS embed.FS

// version 由构建时通过 -ldflags "-X main.version=..." 注入，值来自根目录 VERSION
// 文件。未注入时（如 go run）显示 "dev"。版本号只能由开发者手动修改 VERSION。
var version = "dev"

func main() {
	addr := flag.String("addr", "", "HTTP 监听地址（覆盖 logviewer.json 中的 addr，仅 web 模式生效）")
	dir := flag.String("dir", "", "允许扫描的根工作目录（逗号/分号分隔，合并到本机 local 主机）")
	configPath := flag.String("config", "", "配置文件路径（默认 <exe>/logviewer.json，其次 <cwd>/logviewer.json）")
	hashPw := flag.String("hash-password", "", "生成 bcrypt 密码哈希后退出（用于配置 auth.password）")
	encryptKey := flag.String("key", "", "配置密码解密密钥（也可通过 LOGVIEWER_KEY 环境变量传入）")
	encryptCfg := flag.Bool("encrypt-config", false, "加密配置文件中所有明文密码，写回文件后退出（需配合 -key）")
	decryptCfg := flag.Bool("decrypt-config", false, "解密配置文件中所有加密密码，写回文件后退出（需配合 -key）")
	mode := flag.String("mode", "auto", "运行模式：auto | web | gui。auto 在含 GUI 的构建中默认起窗口，否则起 web 服务")
	flag.Parse()

	// -hash-password：生成 bcrypt 哈希
	if *hashPw != "" {
		h, err := appconfig.HashPassword(*hashPw)
		if err != nil {
			log.Fatalf("生成哈希失败: %v", err)
		}
		fmt.Println(h)
		return
	}

	// 解析密钥：优先 -key，其次环境变量
	key := *encryptKey
	if key == "" {
		key = os.Getenv("LOGVIEWER_KEY")
	}

	// -encrypt-config / -decrypt-config：一次性操作
	if *encryptCfg || *decryptCfg {
		if key == "" {
			log.Fatalf("加密/解密操作需要通过 -key 或 LOGVIEWER_KEY 环境变量提供密钥")
		}
		cfg, cfgPath, err := appconfig.Load(*configPath, nil)
		if err != nil {
			log.Fatalf("加载配置失败: %v", err)
		}
		if *encryptCfg {
			if err := cfg.EncryptPasswords(key); err != nil {
				log.Fatalf("加密失败: %v", err)
			}
			log.Printf("已加密配置文件中的密码: %s", cfgPath)
		} else {
			if err := cfg.DecryptPasswords(key); err != nil {
				log.Fatalf("解密失败: %v", err)
			}
			log.Printf("已解密配置文件中的密码: %s", cfgPath)
		}
		// 只原位替换发生变化的密码标量，保留文件其余注释、格式以及
		// 注释掉的远程主机示例。绝不用全量 Marshal 重写整个文件。
		if err := appconfig.SpliceConfigValues(cfgPath, cfg.PasswordFieldPointers()); err != nil {
			log.Fatalf("写入配置文件失败: %v", err)
		}
		return
	}

	// 静态资源
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("加载静态资源失败: %v", err)
	}

	var extraDirs []string
	if *dir != "" {
		extraDirs = splitList(*dir)
	}

	svc, err := buildService(sub, *configPath, extraDirs, key, guiLogOutput())
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer svc.Close()

	runMode, err := resolveMode(*mode)
	if err != nil {
		log.Fatalf("%v", err)
	}

	switch runMode {
	case modeWeb:
		listenAddr := *addr
		if listenAddr == "" {
			listenAddr = svc.Addr()
		}
		log.Fatal(runWeb(svc, listenAddr))
	case modeGUI:
		if err := runGUI(svc); err != nil {
			log.Fatalf("GUI 启动失败: %v", err)
		}
	}
}

type runMode int

const (
	modeWeb runMode = iota
	modeGUI
)

// resolveMode 根据 -mode 标志与本构建是否支持 GUI 决定实际运行模式。
func resolveMode(m string) (runMode, error) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "web":
		return modeWeb, nil
	case "gui":
		if !supportsGUI() {
			return 0, errors.New("当前构建为 web-only（不含 GUI）；GUI 需在 Windows 上用 -tags gui 编译")
		}
		return modeGUI, nil
	case "auto", "":
		if supportsGUI() {
			return modeGUI, nil
		}
		return modeWeb, nil
	default:
		return 0, fmt.Errorf("mode 只能为 auto、web 或 gui，收到 %q", m)
	}
}

// runWeb 以传统后台服务模式运行：监听 HTTP，处理信号与 SIGHUP 热加载，直到收到中断。
func runWeb(svc *service, listenAddr string) error {
	displayAddr := listenAddr
	if strings.HasPrefix(displayAddr, ":") {
		displayAddr = "127.0.0.1" + displayAddr
	}
	logStartupWarnings(svc.appCfg, listenAddr)

	httpSrv := &http.Server{Addr: listenAddr, Handler: svc.Router()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SIGHUP 热加载（Unix only，Windows 下该信号不存在但不影响编译）
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGHUP)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigCh:
				slog.Info("收到 SIGHUP，正在重新加载配置")
				if err := reloadAndNotify(svc); err != nil {
					slog.Error("配置重载失败", "err", err)
				} else {
					slog.Info("配置重载成功")
				}
			}
		}
	}()

	go func() {
		slog.Info("LogViewer 启动", "version", version, "addr", "http://"+displayAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("服务启动失败", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("收到退出信号，正在关闭（停止日志进程与远程连接）")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP 关闭超时", "err", err)
	}
	return nil
}

// reloadAndNotify 执行一次热加载并把结果应用到 server（更新 auth、日志、通知 WS）。
func reloadAndNotify(svc *service) error {
	newCfg, changed, err := svc.Reload()
	if err != nil {
		return err
	}
	svc.ReinitLogging()
	svc.UpdateAuth(newCfg.Auth)
	if n := svc.NotifyHostsChanged(changed); n > 0 {
		slog.Info("已通知连接因主机配置变更重连", "connections", n)
	}
	slog.Info("配置重载成功", "hosts", len(newCfg.Hosts))
	return nil
}

func authEnabled(cfg *appconfig.AppConfig) bool {
	return cfg.Auth.Enabled && cfg.Auth.Username != ""
}

func logStartupWarnings(cfg *appconfig.AppConfig, addr string) {
	if authEnabled(cfg) {
		ttl := time.Duration(cfg.Auth.SessionTTLMinutes) * time.Minute
		slog.Info("登录认证已启用", "user", cfg.Auth.Username, "ttl", ttl.String())
		pw := cfg.Auth.Password
		if !appconfig.IsBcryptHash(pw) && !cryptoutil.IsEncrypted(pw) {
			slog.Warn("auth.password 为明文，建议用 `logviewer -hash-password <明文>` 生成 bcrypt 哈希后填写")
		}
	} else {
		slog.Info("登录认证未启用（auth.enabled=false 或用户名为空），所有功能可直接访问")
	}
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		h = strings.Trim(h, "[]")
		if h != "127.0.0.1" && h != "localhost" && h != "::1" && !authEnabled(cfg) {
			slog.Warn("监听在非本机地址但未启用登录认证，存在未授权访问风险", "host", h)
		}
	}
}

// buildHosts 从 AppConfig 构造所有 Host 实例（本机 + SSH 远程）。
// 持久化闭包只负责把配置 AST 补丁写回磁盘；config.Manager 自己持有内存中的最新 store，
// 因此不需要回写到某个 AppConfig 快照（避免 reload 后旧闭包让 appCfg 镜像过期）。
func buildHosts(appCfg *appconfig.AppConfig, cfgPath string) ([]host.Host, error) {
	var mu sync.Mutex
	saveCfgFor := func(name string) config.SaveFunc {
		return func(store config.ConfigStore) error {
			mu.Lock()
			defer mu.Unlock()
			return appconfig.PatchHostConfigs(cfgPath, name, store)
		}
	}

	localCfg := appCfg.Hosts["local"]
	local, err := host.NewLocalHost("local", localCfg.Dirs, localCfg.FileExtensions, localCfg.Configs, saveCfgFor("local"))
	if err != nil {
		return nil, err
	}
	local.SetDisplayName(localCfg.DisplayName)
	hosts := []host.Host{local}

	for name, hc := range appCfg.Hosts {
		if name == "local" || hc.SSH == nil {
			// Validate() 已保证非 local 主机必有 ssh；这里防御性跳过。
			continue
		}
		sh, err := host.NewSSHHost(name, *hc.SSH, hc.Platform, hc.Dirs, hc.FileExtensions, hc.Configs, saveCfgFor(name))
		if err != nil {
			return nil, fmt.Errorf("初始化机器 %q 失败: %w", name, err)
		}
		sh.SetDisplayName(hc.DisplayName)
		// 重连成功时累加 Prometheus 指标 + 结构化日志（host 包不依赖 metrics）。
		hostName := name
		sh.SetOnReconnect(func() {
			metrics.IncSSHReconnect(hostName)
			slog.Warn("SSH 断线后重连成功", "host", hostName)
		})
		hosts = append(hosts, sh)
	}
	return hosts, nil
}

// buildHostManager 从 AppConfig 构造一个新的 host.Manager。
func buildHostManager(appCfg *appconfig.AppConfig, cfgPath string) (*host.Manager, error) {
	hosts, err := buildHosts(appCfg, cfgPath)
	if err != nil {
		return nil, err
	}
	return host.NewManager(hosts...)
}

// rebuildHostManager 根据新配置重建 Manager 中的主机集合，保留未变更的主机。
// 返回被替换/移除的主机别名。
func rebuildHostManager(hm *host.Manager, newCfg *appconfig.AppConfig, cfgPath string) ([]string, error) {
	hosts, err := buildHosts(newCfg, cfgPath)
	if err != nil {
		return nil, err
	}
	return hm.Rebuild(hosts), nil
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
