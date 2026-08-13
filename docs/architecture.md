# 架构设计

## 设计原则

**一切操作都是命令，Go 只是外壳。**

日志的读取、跟踪、过滤、编码转换全部由操作系统原生命令完成，Go 进程不解析、
不匹配、不缓冲日志内容。它只做四件事：

1. 把前端配置翻译成一条原生命令管道；
2. 启动该命令并把 stdout/stderr 通过 WebSocket 转发给浏览器；
3. 管理进程生命周期（开始 / 停止 / 断开清理 / 杀进程组）；
4. 提供目录浏览、配置持久化、文件导出等辅助 HTTP 接口。

这样做的收益：

- **性能**：`tail` / `grep` / `awk` / PowerShell 都是为流式处理大文件优化过的成熟工具，
  跟踪大日志文件时内存恒定、延迟低。
- **跨平台语义一致**：Unix 和 Windows 各自用本机最顺手的工具，Go 侧只翻译参数。
- **可靠**：`tail -F` 跟 inode，天然兼容 logrotate；`Get-Content -Wait` 同理。
- **安全**：命令构建使用参数化引用，不把用户输入拼进 `sh -c` 的不可控位置。

## 模块划分

```
internal/
├── cmdbuild/   # 把 LogConfig 翻译成各平台的命令脚本（纯字符串构建，无副作用）
├── procmgr/    # 子进程的启动、stdout/stderr 读取、节流批量、进程组查杀
├── config/     # LogConfig 结构 + configs.json 的 CRUD 持久化
└── server/     # Gin 路由：目录、配置 API、导出、WebSocket
```

### cmdbuild —— 命令构建

- `BuildView(mode, file, encoding, limit, filter)` → `Command{Shell, Script}`
- `BuildOrigin(file)` → 原样导出文件字节
- `BuildExport(...)` → 过滤导出（等价于 static 模式的 View）
- `BuildCmd()` 把 `Command` 转成 `*exec.Cmd`，Unix 走 `sh -c`，Windows 走
  `powershell -NoProfile -NonInteractive -Command`。

平台分支：

| 场景        | Unix                                   | Windows                                          |
| ----------- | -------------------------------------- | ------------------------------------------------ |
| 实时跟踪    | 指定行 `tail -F -n N file`；不指定 `{ cat file; tail -F -n 0 file; }` | `Get-Content -Wait -Tail N`（见下）       |
| 静态全量    | `cat file`                             | `[IO.File]::ReadLines(path, enc)`                |
| 静态末 N 行 | `tail -n N file`                       | `Get-Content -Tail N`                            |
| GBK 解码    | `iconv -f GBK -t UTF-8`                | `-Encoding OEM` / `.NET GetEncoding('GBK')`      |
| 时间范围    | `awk` 字符串比较 + 状态保留续行        | `Where-Object` + `$script:_keep` 状态            |
| 内容过滤    | `grep -E` / `grep -v -F`（line-buffered） | `Select-String` / `-NotMatch -SimpleMatch`    |
| 上下文      | `grep -B N -A N`                       | `Select-String -Context B,A`                     |

**Windows 跟踪**：当指定了 `limit`，命令是

```powershell
& { Get-Content <file> -Wait -Tail N }
```

不指定行数时用 `Get-Content -Wait`。Unix 不指定行数时用 `{ cat; tail -F -n 0; }`
两步式，保证先输出全量历史再跟随新增。

### procmgr —— 进程与流读取

- `Start(cmd, outFn, errFn, doneFn)` 启动进程，开启三个 goroutine：
  - stdout 读取协程：按行读 + **独立定时协程节流批量**（默认 40ms 或满 512 行
    flush 一次），避免高频日志产生 WebSocket 消息风暴。
  - stderr 读取协程：逐行回调 `errFn`。
  - Wait 协程：**先等 stdout/stderr 读取协程全部结束（含最后一次 flush），再
    `cmd.Wait()`**，随后从 map 移除并回调 `doneFn`。保证 `done` 关闭后绝无在途的
    `outFn` 调用，杜绝"停止后仍向外写"。
- 关键设计（详见 [lessons.md](lessons.md)）：
  - flush 逻辑**不**放在阻塞 `ReadString` 的同一循环里定时；用独立 ticker 协程刷新，
    batch 由 mutex 保护，`outFn` 由另一把 `flushMu` 串行化。
  - 退出时关闭 `stopFlush` 并 `Wait` ticker 协程，确保无并发/在途 flush。
- `Stop(id)` 杀进程树并等待输出 flush + 进程回收；`StopAll()` 在服务关闭时清理全部残留。
- 进程树查杀通过 build tag 区分平台：Unix 用 `Setpgid` 建独立进程组，再
  `syscall.Kill(-pgid, SIGKILL)` 连同 `sh` 及其子进程（tail/grep/iconv）一起杀；
  Windows 用 `taskkill /T /F /PID` 终止整个进程树（powershell 可能派生子宿主，
  不能只 `Process.Kill` 顶层进程）。

### server —— HTTP / WebSocket

- 目录浏览：`/api/dir/roots`、`/api/dir/list?path=`，懒加载单层节点，
  只展示目录和 `.log`/`.out` 文件。
- 配置 CRUD：`/api/config/list|get|save|delete|rename|setdefault|preview`。
- 导出：`GET /api/file/download/origin`（原始字节，带 `Content-Length`）、
  `POST /api/file/download/filter`（按当前表单过滤，流式输出）。
- WebSocket：`/ws`，上行 `start`/`stop`/`ping`，下行 `log`/`error`/`status`。

## 请求流（一次实时跟踪）

```
浏览器                      Go server                    原生命令
  │  start{file,config}       │                            │
  ├──────────────────────────►│  校验路径 / 拼装命令        │
  │                           ├─── BuildView().BuildCmd() ┤
  │                           ├─── procmgr.Start() ──────►│ tail -F | grep ...
  │                           │◄──── stdout batches ───────┤
  │  {type:log,data:"..."}    │  (40ms 节流批量)           │
  │◄──────────────────────────┤                            │
  │   ...追加一行...          │                            │
  │                           │◄──── 新行 ─────────────────┤
  │  {type:log,data:"..."}    │                            │
  │◄──────────────────────────┤                            │
  │  stop                     │  kill 进程组               │
  ├──────────────────────────►├─── procmgr.Stop() ───────► X
```

## 前端

- 纯原生 HTML/CSS/JS，无构建步骤、无 npm。
- 终端用本地 vendor 的 **xterm.js**（+ fit addon），所有前端依赖都在 `static/vendor/`，
  运行时不联网。
- 行号用「双 xterm」方案：左侧一个只读、不可聚焦的 gutter 终端，按主终端的
  `buffer.active.viewportY` 做绝对滚动同步。
- 时间选择器用 **flatpickr**（中文 locale），按天/时/分/秒四种粒度切换日期格式。
- 主题用 CSS 变量 + `data-theme`，明/暗两套，xterm 的 theme 选项同步切换。

## 安全边界

- **根目录限制**：服务维护允许访问的根目录列表（`-dir` 指定），任何文件请求都经过
  `resolveAndCheck`：`Abs` → `Clean` → 对每个 root 做 `Rel`，跳出根目录一律拒绝。
- **命令注入防护**：文件路径和模式串用平台专属引号转义（Unix `'...'` + `'\''`，
  PowerShell `'...'` + `''`），通过 `-Command` / `sh -c` 的参数位传入。
- 默认只监听本机地址；需要远程访问时应放在带认证的反向代理之后。
