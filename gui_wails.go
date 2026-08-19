//go:build gui && windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"logviewer/internal/appconfig"
)

// exeDirOrCwd 返回可执行文件所在目录，失败时退回当前工作目录。
func exeDirOrCwd() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

// supportsGUI 报告当前构建是否含桌面 GUI（Windows + -tags gui 时为真）。
func supportsGUI() bool { return true }

// loadingHTML 是 WebView 加载内置 Gin 前的占位页。
// 真正的前端由 Gin 在 127.0.0.1 随机端口提供；就绪后用 JS 跳转到该地址。
const loadingHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>LogViewer</title><style>
html,body{height:100%;margin:0;background:#1e1e2e;color:#cdd6f4;
font-family:Segoe UI,Microsoft YaHei,sans-serif;display:flex;align-items:center;justify-content:center}
.box{text-align:center}
.sp{width:34px;height:34px;border:3px solid #45475a;border-top-color:#89b4fa;border-radius:50%;
margin:0 auto 18px;animation:r 0.9s linear infinite}
@keyframes r{to{transform:rotate(360deg)}}
</style></head><body><div class="box"><div class="sp"></div><div>正在启动 LogViewer…</div></div></body></html>`

// loadingFS 提供占位页作为 Wails 初始 Assets。
func loadingFS() fs.FS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(loadingHTML)},
	}
}

// runGUI 以桌面客户端模式运行：在 127.0.0.1 随机端口启动内置 Gin，
// Wails 窗口的 WebView 加载该地址。Gin 仅监听 loopback，外部不可达。
func runGUI(svc *service) error {
	var (
		ln          net.Listener
		navigateURL string
		navOnce     sync.Once
	)

	err := wails.Run(&options.App{
		Title:     "LogViewer",
		Width:     1280,
		Height:    800,
		MinWidth:  960,
		MinHeight: 600,
		Logger:    logger.NewFileLogger(filepath.Join(exeDirOrCwd(), "wails.log")),
		// Wails 默认把 WebView 指向内部 asset server（http://wails.localhost）。
		// 先喂一个"启动中"占位页，Gin 就绪后在 OnDomReady 里跳转过去。
		AssetServer: &assetserver.Options{Assets: loadingFS()},
		OnStartup: func(ctx context.Context) {
			// 直接保留 listener：消除"监听后关闭再 Run"的端口竞态，
			// 同时拿到确切端口传给 WebView。GUI 模式固定 loopback，外部不可达。
			var err error
			ln, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				slog.Error("GUI 监听本地端口失败", "err", err)
				return
			}
			navigateURL = "http://" + ln.Addr().String() + "/"
			slog.Info("GUI 内置 Gin 监听", "addr", navigateURL)

			go func() {
				serveErr := http.Serve(ln, svc.Router())
				// 关窗时 ln.Close() 会让 Serve 返回 net.ErrClosed，属正常退出。
				if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
					slog.Error("内置服务异常", "err", serveErr)
				}
			}()
		},
		OnDomReady: func(ctx context.Context) {
			// OnDomReady 在【每次】页面加载完成时都会触发：占位页触发一次，
			// 跳到 Gin 后 Gin 页面又会触发一次。必须用 sync.Once 保证只跳一次，
			// 否则会形成"加载→跳转→加载→跳转"的无限重载，页面持续闪烁、WS 无法建立。
			// 等待与轮询放到 goroutine 里，避免阻塞 WebView UI 线程；同时容忍
			// 占位页 DOM ready 早于 OnStartup 设置 ln 的竞态。
			go navOnce.Do(func() {
				addr := waitForListener(&ln, 15*time.Second)
				if addr == "" {
					slog.Error("内置服务未在规定时间内监听", "url", navigateURL)
					_, _ = runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
						Type:    runtime.ErrorDialog,
						Title:   "LogViewer",
						Message: "内置服务启动超时，请查看日志后重试。",
					})
					return
				}
				// location.replace 跳到内置 Gin，之后该前端页的 OnDomReady 不再触发跳转。
				runtime.WindowExecJS(ctx, "window.location.replace('"+navigateURL+"')")
			})
		},
		OnShutdown: func(ctx context.Context) {
			// 先关闭 listener 让 http.Serve 返回，再统一回收后端资源
			// （follow 会话、PowerShell/tail 进程、SSH 连接）。
			if ln != nil {
				_ = ln.Close()
			}
			svc.Close()
		},
	})
	return err
}

// waitForListener 等待 ln 被 OnStartup 赋值且 /readyz 返回 200，返回监听地址。
// 轮询 /readyz（而非 /healthz）：/readyz 仅确认 HTTP 服务本身就绪，不探测 SSH 主机。
// 重启电脑后网络/VPN 可能尚未就绪，SSH 连接超时（默认 10s）不应阻塞 GUI 窗口加载。
// 超时返回空串。
func waitForListener(lnp *net.Listener, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		if *lnp == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		addr := (*lnp).Addr().String()
		resp, err := client.Get("http://" + addr + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return addr
			}
		}
		time.Sleep(80 * time.Millisecond)
	}
	return ""
}

// guiLogOutput 在 GUI 模式下把所有控制台输出（os.Stdout / os.Stderr、
// gin 访问日志、slog、标准库 log）全部重定向到日志文件，并返回该文件供
// slog 初始化使用。-H windowsgui 下没有控制台，不重定向输出会全部丢失。
//
// 日志路径优先取配置文件中的 log_file（相对路径以 exe 目录为基准），
// 未配置则使用 exe 同目录下的 logviewer-gui.log。每次启动截断覆盖旧文件。
func guiLogOutput(configPath string) io.Writer {
	logPath := resolveLogFilePath(configPath)

	// 确保日志目录存在；失败时回退到默认路径。
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		fallback := defaultLogPath()
		if fallback != "" && filepath.Dir(fallback) != filepath.Dir(logPath) {
			if err2 := os.MkdirAll(filepath.Dir(fallback), 0o755); err2 == nil {
				logPath = fallback
			}
		}
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil
	}

	// 重定向进程级 stdout / stderr，覆盖所有直接 fmt.Print / os.Stderr 的调用。
	os.Stdout = f
	os.Stderr = f

	// gin 访问日志与错误输出也指向文件（gin.Recovery 堆栈走 ErrorWriter）。
	gin.DefaultWriter = f
	gin.DefaultErrorWriter = f

	_, _ = fmt.Fprintf(f, "===== LogViewer GUI 启动 %s =====\n", time.Now().Format(time.RFC3339))
	return f
}

// resolveLogFilePath 确定日志文件路径：优先配置中的 log_file，其次默认路径。
// 相对路径相对于可执行文件所在目录解析。
func resolveLogFilePath(configPath string) string {
	if cfgLogFile := appconfig.ReadLogFile(configPath); cfgLogFile != "" {
		// 相对路径以 exe 目录为基准（与配置文件查找逻辑一致）
		if !filepath.IsAbs(cfgLogFile) {
			if exe, err := os.Executable(); err == nil {
				cfgLogFile = filepath.Join(filepath.Dir(exe), cfgLogFile)
			}
		}
		return filepath.Clean(cfgLogFile)
	}
	return defaultLogPath()
}

// defaultLogPath 返回默认日志文件路径：可执行文件同目录下的 logviewer-gui.log。
func defaultLogPath() string {
	return filepath.Join(exeDirOrCwd(), "logviewer-gui.log")
}
