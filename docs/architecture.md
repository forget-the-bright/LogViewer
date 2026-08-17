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
├── appconfig/  # logviewer.json 加载(JSONC)、默认模板生成、旧配置迁移、密码哈希、hujson AST 局部补丁、密码加解密
├── cryptoutil/ # AES-256-GCM 密码加解密（enc:v1: 前缀格式）
├── host/       # 机器抽象：Host 接口 + LocalHost（本机 os/exec）+ SSHHost（SSH/SFTP + 远程命令）
├── cmdbuild/   # 把 LogConfig 翻译成各平台的命令脚本（纯字符串构建，无副作用）
├── procmgr/    # 子进程的启动、stdout/stderr 读取、节流批量、进程组查杀
├── config/     # LogConfig 结构 + 预设 CRUD（内存 + SaveFunc 持久化钩子）
└── server/     # Gin 路由：机器列表、目录、配置 API、导出、WebSocket、热加载
```

### host —— 机器抽象

server 层不直接调用 `os.*` / `exec.*` / `runtime.GOOS`，而是通过 `Host` 接口：

- `Name() / Platform() / Dirs() / Configs() / Capabilities()`
- `ResolvePath(p)` —— 路径穿越校验（含符号链接逃逸检测）
- `Ls / Stat / Open` —— 目录浏览与文件读取
- `Run(cmdbuild.Command)` —— 返回 `procmgr.Process`，由 procmgr 统一管控

`LocalHost` 是本机实现。`SSHHost` 通过 SSH+SFTP 访问远程机器：密码认证、known_hosts
TOFU（默认写入 `~/.ssh/known_hosts`，首次落盘后严格校验）、`uname -s`/`ver` 自动探测平台
（Win32-OpenSSH 默认 shell 为 cmd.exe，用裸 `ver` 而非 `cmd /c ver`，避免嵌套引号 bug，
另以 PowerShell 兜底）、keepalive 保活与断线重连。远程路径校验不依赖本机 `filepath`，
而是按目标平台分隔符做词法清洗 + SFTP `RealPath` 解析符号链接，防止跨平台路径穿越与软链逃逸。

远程命令执行由 `sshProc` 实现（`internal/host/ssh_proc.go`）：把 cmdbuild 生成的脚本
包成 `sh -c '<script>'`（Unix）或 `<ps> -NoProfile -NonInteractive -EncodedCommand
<b64>`（Windows，UTF-16LE base64 绕过 cmd 引号问题；`<ps>` 在连接初始化时探测，
远端有 `pwsh` 则用 7+，否则回退 `powershell` 5.1），在 SSH session 上启动，stdout/stderr
交给 procmgr 同一套读取/节流逻辑。远端进程树查杀靠 **PID 标记**：脚本开头向 stderr 打印
`LV_PID=<pid>`（Unix 用 `printf '%s' "$"`，Windows 用 `$PID`），`pidFilterReader`
拦截首行解析出 PID，停止时另开一条 SSH 连接执行整组查杀（Unix `kill -KILL -<pgid>`，
Windows `taskkill /T /F /PID`），再关闭当前 session，避免 OpenSSH 信号不可靠导致
tail/grep 残留。

`Capabilities()` 返回远端是否具备 `tail/cat/grep/awk/iconv`（连接时批量
`command -v` 探测；Windows 恒为 true）。最小化容器可能缺 `iconv`/`awk`，服务端
`checkCaps` 在构建命令前拦截并返回可读错误，前端据 `/api/h/:host/capabilities`
禁用 GBK 选项（需 iconv）和时间范围过滤（需 awk）。

业务路由统一加 `/api/h/:host/` 前缀，WebSocket 连 `/ws?host=<alias>`，为本机和远程
共用同一套逻辑。

### cmdbuild —— 命令构建

- `BuildView(mode, file, encoding, limit, filter)` → `Command{Shell, Script}`
- `BuildOrigin(file)` → 原样导出文件字节
- `BuildExport(...)` → 过滤导出（等价于 static 模式的 View）
- `BuildCmd()` 把 `Command` 转成 `*exec.Cmd`，Unix 走 `sh -c`，Windows 走
  `<ps> -NoProfile -NonInteractive -WindowStyle Hidden -Command`，其中 `<ps>`
  在本机优先用 `pwsh`（PowerShell 7+，启动比 5.1 快约 5 倍），未安装时回退
  `powershell`（5.1）。

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

- `Start(proc, outFn, errFn, doneFn)` 启动一个实现了 `Process` 接口的进程
  （本机 `localProc` 包装 `*exec.Cmd`；远程 `sshProc` 包装 `*ssh.Session`），开启三个 goroutine：
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

- 机器列表：`GET /api/hosts`（别名、平台、在线状态，供顶栏切换器）。
- 能力探测：`GET /api/h/:host/capabilities`。
- 目录浏览：`/api/h/:host/dir/roots`、`/api/h/:host/dir/list?path=`，懒加载单层节点，
  只展示目录和 `.log`/`.out` 文件。
- 配置 CRUD：`/api/h/:host/config/list|get|save|delete|rename|setdefault|preview`，每台机器独立预设。
- 导出：`GET /api/h/:host/file/download/origin`（原始字节，带 `Content-Length`）、
  `POST /api/h/:host/file/download/filter`（按当前表单过滤，流式输出）。
- WebSocket：`/ws?host=<alias>`，上行 `start`/`attach`/`stop`/`ping`，下行 `log`（带 `seq`）
  /`error`/`status`（`running`/`stopped`/`waiting`/`alive`）/`notice`（`rotate`/`truncate`/`gap`）
  /`reconnect`（主机热更后通知前端迁移连接）。
- 可观测性：`GET /healthz`（各主机连通状态，免鉴权）、`GET /metrics`（Prometheus，免鉴权）。
  指标见 `internal/metrics`：`logviewer_ws_connections`、`logviewer_log_processes`、
  `logviewer_ssh_reconnects_total`、`logviewer_export_bytes_total`、`logviewer_log_bytes_sent_total`。

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

### 断线会话解耦与补齐（follow 模式）

follow 跟踪的会话（`viewSession`）与单个 WebSocket 连接解耦，进程不再随 WS 断开被杀：

- 启动 follow 时生成 `sessionID`，随 `{type:"status",status:"running",sessionID,seq}` 下发；
  每批日志分配单调递增 `seq`，写入 2MB 有界环形缓冲（按字节淘汰最旧批次）后再下发。
- WS 断开：会话进入宽限期（`session_grace_seconds`，默认 45s），进程继续运行，输出照常入环
  但不绑定连接。宽限到期才 `procmgr.Stop` 并销毁会话。
- 重连：前端发 `{action:"attach",sessionID,lastSeq}`。会话仍存活则取消宽限定时器，在
  `writeMu` 下补发所有 `seq>lastSeq` 的批次（保证补发帧先于后续实时帧，不乱序）；
  若 `lastSeq < oldestSeq`（早期批次已被淘汰），先下发 `{type:"notice",kind:"gap"}` 提示部分丢失。
  会话已失效则回退到全量 `start`。
- 用户主动 stop / 切换文件 / 切换主机：立即销毁会话（不进宽限）。
- 静态（非 follow）模式仍是一次性读取、连接绑定，断了即重来（无补齐意义）。

stderr 经 `classifyStderr` 分类：`tail -F` 的"文件出现/建立"为良性噪声忽略；"被替换/轮转"与
"文件截断"转为 `notice`（`rotate`/`truncate`）由前端显示非红色提示条；其余（grep/awk/iconv 报错等）
为真实错误染红。

## 前端

- 纯原生 HTML/CSS/JS，无构建步骤、无 npm。
- 终端用本地 vendor 的 **xterm.js**（+ fit addon），所有前端依赖都在 `static/vendor/`，
  运行时不联网。
- 行号用「双 xterm」方案：左侧一个只读、不可聚焦的 gutter 终端，按主终端的
  `buffer.active.baseY + length` 绝对坐标计算新增行（scrollback 封顶后仍正确），
  滚动位置跟随 `buffer.active.viewportY` 同步。
- 时间选择器用 **flatpickr**（中文 locale），按天/时/分/秒四种粒度切换日期格式。
- 主题用 CSS 变量 + `data-theme`，自动（跟随系统 `prefers-color-scheme`）/明/暗三态，
  xterm 的 theme 选项同步切换。
- 快捷键经 xterm 的 `attachCustomKeyEventHandler` 放行 Ctrl+F/Ctrl+C 等浏览器组合键（返回
  `false` 让 xterm 不在捕获阶段 `preventDefault`），从根上修复搜索/复制被吞的问题。
- 偏好（主题、语言、字号/行高密度、scrollback、侧栏宽度）统一存 `localStorage` 的
  `logviewer-prefs` JSON；文案经 `static/i18n.js`（中/英，键集合由 `i18n.test.mjs` 校验）。

## 安全边界

- **根目录限制**：每个 Host 维护允许访问的根目录列表（来自 `logviewer.json` 的 `dirs`
  与 `-dir` 合并），任何文件请求都经过 `ResolvePath`：`Abs` → `Clean` → 对每个 root
  做 `Rel`，跳出根目录一律拒绝。不能用字符串前缀判断（`/root` 会被 `/root-evil` 绕过）。
- **符号链接逃逸检测**：`ResolvePath` 对路径做 `EvalSymlinks`（文件不存在时退化到父目录），
  解析出的真实路径若跳出根目录也拒绝，防止 root 内软链指向外部敏感文件。
- **命令注入防护**：文件路径和模式串用平台专属引号转义（Unix `'...'` + `'\''`，
  Windows 用 `-EncodedCommand` 传 UTF-16LE base64，完全绕过 cmd/PowerShell 引号转义），
  不经由字符串拼接进 shell。
- **登录认证（可选）**：`auth.enabled=true` 时，除 `/api/login`、`/api/auth/status`
  外的所有 `/api/**` 都要过会话中间件，WebSocket 在 Upgrade 前校验 Cookie 并做同源校验。
  会话 token 用 crypto/rand 生成、仅存内存、滑动续期；登录按 IP 限流（60 秒 5 次）。
  默认关闭，关闭时不做任何登录校验。
- 默认只监听本机地址；绑定非本机地址且未启用认证时，启动会打印安全警告。

## 配置文件与持久化

### JSONC + hujson AST 局部补丁

`logviewer.json` 支持 `//` 和 `/* */` 注释（通过 tailscale/hujson 解析）。唯一的编程式写入场景
是保存过滤预设（`hosts.<name>.configs`）。为避免整体 `json.Marshal` 重写导致注释全部丢失，
`PatchHostConfigs` 通过 hujson AST 的 JSON Pointer（`/hosts/<name>/configs`）定位目标值的
字节区间 `[StartOffset, EndOffset)`，然后做字节拼接替换。其余字段的注释、格式、顺序原封不动。

### 密码加密（AES-256-GCM）

`internal/cryptoutil` 提供对称加密：

- 用户 passphrase 经 SHA-256 派生为 32 字节密钥；
- 每次加密生成随机 96-bit nonce，AES-256-GCM 加密，输出 `enc:v1:<base64(nonce||ciphertext)>`；
- 非 `enc:v1:` 前缀的字符串视为明文，原样透传（兼容未加密配置）。

`AppConfig.EncryptPasswords / DecryptPasswords` 对 `auth.password`（bcrypt 哈希跳过）和所有
`ssh.password` 批量加解密。启动时若检测到加密密码，必须通过 `-key` 或 `LOGVIEWER_KEY` 环境变量
提供密钥，在内存中解密后运行；`-encrypt-config` / `-decrypt-config` 是写回磁盘的一次性操作。

### 配置热加载

两种手动触发方式：

1. `POST /api/reload`（前端"重载配置"按钮）；
2. Unix 下发送 `SIGHUP` 信号（Windows 仅 API）。

`reloadCfg` 在锁内重新读取配置文件、解密密码、调用 `host.Manager.Rebuild` 原子替换主机集合。
`Rebuild` 通过连接指纹（name + host:port + username + password 等关键 SSH 参数）判断主机是否
变更——指纹相同的实例直接保留，正在跟踪的日志不会中断；新增/删除/配置变更的主机才创建或关闭，
`Rebuild` 返回被替换/移除的主机别名，`srv.NotifyHostsChanged` 给这些主机上的 WS 连接下发
`{"type":"reconnect"}` 并关闭连接，前端 onclose 立即重连拿到新 Host 实例（抑制离线横幅）。
LocalHost 始终保留（仅通过 `UpdateDirs` 更新根目录）。Reload 后通过 `srv.UpdateAuth` 热更新
认证配置（开关/用户名变化时清空所有会话）。

不做文件监听自动 reload，避免读到半写入状态。

## 前端交互细节

- 顶栏机器列表每 10 秒静默刷新（`document.hidden` 时跳过），更新在线/离线状态文字；新增/删除
  的主机动态增删选项，不重建 DOM 避免闪烁。
- "刷新"按钮同时刷新机器列表和当前目录树。
- WebSocket 断线时终端顶部显示红色横幅（脉动动画），指数退避重连（1s → 2s → 4s → ... → 最大
  30s），重连成功后横幅消失；"立即重连"按钮可手动触发。
- 服务端 WebSocket 有 30s ping / 90s pong 超时保活，防止半开 TCP 连接永久挂起。

## 优雅关闭

收到 SIGINT / SIGTERM 后：

1. `http.Server.Shutdown` 停止接收新连接、等待进行中的 HTTP 请求结束（5 秒超时）；
2. `Server.Close()` 先在注册表锁内逐一 `viewSession.destroy()`（取消宽限定时器、杀掉
   follow 进程并从注册表移除），再 `procmgr.StopAll()` 兜底回收静态进程（远程连同进程组
   一起查杀，避免 tail/管道孤儿），最后 `host.Manager.Close()` 关闭所有 SSH 客户端与
   keepalive。顺序很重要：必须先杀进程再断 SSH，否则查杀命令发不出去。
