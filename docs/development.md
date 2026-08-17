# 开发指南

## 环境要求

- Go 1.26+
- 前端无需 Node/npm：所有第三方库（xterm.js、flatpickr）都已本地 vendor 到
  `static/vendor/`，随 `go:embed` 打包。
- Windows 上开发需要 PowerShell（`powershell.exe`，系统自带）；
  Linux/macOS 需要 `tail`、`cat`、`awk`、`grep`、`iconv`。

## 常用命令

```bash
# 运行（默认 :8080，根目录为当前工作目录）
go run .

# 指定端口和扫描根目录
go run . -addr 127.0.0.1:9000 -dir "D:\logs,C:\tomcat\logs"

# 编译当前平台（版本号取自 VERSION，未注入时为 dev）
go build -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/logviewer .

# 跑单元测试
go test ./...

# 交叉编译
GOOS=linux GOARCH=amd64 go build -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/logviewer .
```

> 版本号唯一来源是根目录 `VERSION` 文件，**只能由开发者手动修改**，AI/自动化不得改动。

Windows 下一键全平台打包（自动读取 `VERSION`）：

```powershell
.\build_all_platforms.ps1
# 产物在 dist/，包含 windows/linux/darwin × amd64/arm64 的 zip/tar.gz
```

## 代码结构速查

| 你想改什么                 | 去哪                                                   |
| -------------------------- | ------------------------------------------------------ |
| logviewer.json 结构/加载   | `internal/appconfig/`                                  |
| 机器抽象 / 本机实现 / 路径校验 | `internal/host/`                                   |
| 命令在各平台怎么拼         | `internal/cmdbuild/cmdbuild.go`（platform 由 Host 传入） |
| 过滤正则 / 时间区间算法    | `internal/cmdbuild/filter.go`                          |
| 子进程读取 / 节流 / 查杀   | `internal/procmgr/procmgr.go` + `procsys_*.go`         |
| 配置字段与预设 CRUD        | `internal/config/config.go`                            |
| HTTP 路由 / 机器分发       | `internal/server/server.go`                            |
| WebSocket 协议 / 会话流程  | `internal/server/ws.go`                                |
| 目录浏览 API               | `internal/server/dir.go`（走 Host.Ls/ResolvePath）     |
| 导出                       | `internal/server/export.go`                            |
| 启动参数 / embed           | `main.go`                                              |
| 前端所有交互               | `static/app.js`、`static/index.html`、`static/style.css` |

## 调试技巧

### 直接观察命令输出

后端只是命令的外壳，排查"过滤/跟踪不对"时，最快的方法是把实际拼出来的命令
拿到终端里直接跑：

- 把配置里的 `log_commands` 设为 `true`（支持热加载），服务端会以 INFO 级别打印
  每条查询/导出/正则校验命令的 `shell`、`platform` 和完整 `script`，直接复制即可复现。
- Unix：在日志目录手动执行 `tail -F ... | awk ... | grep ...`。
- Windows：把 `cmdbuild.go` 里拼出的 PowerShell 脚本粘到 PowerShell 窗口执行。

如果原生命令输出就是错的，问题在命令拼装（`cmdbuild`）；如果原生命令输出正确
但前端不对，问题在 `procmgr`（读取/节流）或 `ws.go`（转发）或前端。

### WebSocket 调试

浏览器 DevTools 的 Network → WS 可以直接看收发的 JSON 帧：

- 上行：`{"action":"start","filePath":"...","config":{...}}` / `{"action":"stop"}`
- 下行：`{"type":"log","data":"..."}` / `{"type":"error","msg":"..."}` /
  `{"type":"status","status":"running|stopped|waiting"}`

### 验证无残留进程

停止跟踪或关闭页面后，确认没有漏网的子进程：

```bash
# Linux/macOS
ps aux | grep -E 'tail|grep|awk'

# Windows
tasklist | findstr powershell
```

杀进程逻辑在 `procsys_unix.go` / `procsys_windows.go`，Unix 杀整个进程组，
Windows 用 `taskkill /T /F`。

## 前端开发注意

- 改完 `static/` 里的文件，重新 `go run .` / `go build` 即可（资源是 embed 进二进制的）。
- xterm 行号 gutter 是双终端方案，改终端尺寸/字体时要同步 `GUTTER_COLS`、
  `FONT_SIZE` 并检查 `fitTerm()` / `syncGutter()`。
- 主题色集中在 `style.css` 的 `:root` / `[data-theme="light"]` CSS 变量，以及
  `app.js` 里的 `XTERM_THEMES`，两处要一起改。
- 时间选择器粒度切换逻辑在 `applyTimePrecision()`，会销毁并重建 flatpickr 实例
  以保留已选日期。

## 提交前检查清单

- [ ] `go build ./...` 通过
- [ ] `go test ./...` 通过
- [ ] Windows 跟踪 + 静态两种模式都正常
- [ ] 停止/断开后无残留 powershell/tail 进程
- [ ] 改了过滤逻辑时，用大文件验证延迟和内存
