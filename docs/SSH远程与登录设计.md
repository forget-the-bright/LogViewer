# SSH 远程机器 / 登录认证 / 可拖拽侧栏 — 需求设计文档

> 状态：**已全部实现（阶段一~四）**，本文保留为设计记录。实现与设计的主要偏差见文末「实现偏差」。
> 适用版本：v0.0.4
> last changed：2026-08-16

本文档描述在现有「Go 只是外壳、一切操作都是原生命令」架构上，新增 SSH 远程日志查看、
内置登录认证、机器切换、可拖拽侧栏四块能力的完整方案。包含配置格式、模块划分、接口契约、
前端改动，以及设计评审中识别出的安全/健壮性漏洞与规避措施。

---

## 1. 需求

1. **统一配置文件 `logviewer.json`**：启动时在启动目录生成一份带注释的配置文件。
   - 查找顺序：`<可执行文件目录>/logviewer.json` → `<当前工作目录>/logviewer.json`；
     都没有则在 **可执行文件目录**生成模板。
   - 支持 `-config <path>` 显式指定。
   - 文件内容含注释（JSONC：`//` 与 `/* */`），注释标注每一项的含义。
   - 包含：监听地址、登录账号密码、每台机器的 SSH 连接信息、扫描目录、过滤预设。
   - 扫描目录与命令行 `-dir` **去重合并**（`-dir` 只能加给本机 `local`）。
2. **多机器模型**：顶层按机器别名组织，`local` 表示本机（无 SSH 字段）；其它机器填
   IP、端口、账号、密码。每台机器有自己独立的扫描目录和过滤预设。
3. **平台自动探测**：SSH 连通后自动识别远程是 Linux / macOS / Windows，据此选择底层
   原生命令（`tail/grep/awk/iconv` vs `Get-Content/Select-String`）。
4. **界面**：
   - 顶栏可切换机器；
   - 左侧目录树支持拖拽改宽度（当前固定 260px，长文件名显示不全）；
   - 登录页：配置了账号密码时，未登录不能进入日志界面，登录状态需保持。
5. 修正现有文档中与代码不符的过时描述。

### 1.1 已确认的设计决策

| 项 | 决策 |
| --- | --- |
| 配置查找位置 | exe 目录优先，其次 cwd |
| 过滤预设作用域 | 每台机器独立一套 |
| SSH 认证方式 | 仅密码（私钥/agent 后续可扩展） |
| 远程目录浏览 | SFTP（日志读取仍走 SSH exec 原生命令管道） |
| 登录密码存储 | 支持明文与 bcrypt 哈希，推荐哈希 |

---

## 2. 现状回顾

当前实现要点（详见 `docs/architecture.md`）：

- `main.go` 解析 `-addr`/`-dir`，配置目录写死 `<exe>/config/`，过滤预设存 `configs.json`。
- `internal/cmdbuild` 用 `runtime.GOOS` 在编译期决定 Unix/Windows 命令分支；
  `Command.BuildCmd()` 直接返回本机 `*exec.Cmd`。
- `internal/procmgr` 管理本机子进程：`Start(*exec.Cmd,...)`、独立 ticker 协程批量 flush、
  `Stop` 杀进程组（Unix `Setpgid`+`Kill(-pgid)`，Windows `taskkill /T`）。
- `internal/server` 持有本机 `roots []string`，`resolveAndCheck` 用 `filepath.Abs/Clean/Rel`
  做路径穿越校验；路由 `/api/dir/*`、`/api/config/*`、`/api/file/download/*`、`/ws`
  全部无认证；WebSocket `CheckOrigin` 恒为 true。
- 前端顶栏 `#rootSelect` 只列本机根目录，`#treePanel` 宽度固定 260px。

**核心约束**：SSH 远程能力不能破坏「Go 只是外壳」原则——远程日志的读取/过滤仍由远程机器的
原生命令完成，Go 侧只是把命令通过 SSH 通道发出去并把字节流转给浏览器。

---

## 3. 配置文件设计

### 3.1 文件位置与生成

- 启动时按 `<-config>` → `<exe>/logviewer.json` → `<cwd>/logviewer.json` 顺序查找。
- 都不存在时，在 **exe 目录**生成内置模板（见 3.2），文件权限 `0600`（含 SSH 密码）。
- 旧版升级：若发现 `<exe>/config/configs.json` 且没有 `logviewer.json`，把其中
  `configs`/`default_name` 迁移到 `hosts.local.configs`，旧文件改名 `configs.json.bak`。
- 配置热加载不在本期范围；修改配置需重启。

### 3.2 模板（带注释）

```jsonc
{
  // HTTP 监听地址。默认只监听本机回环。
  // 若绑定到 0.0.0.0 且未配置 auth，启动会打印警告并要求加 -insecure-allow-remote。
  "addr": ":8080",

  // 登录认证。username 留空表示不启用登录页。
  // password 推荐填 bcrypt 哈希（$2a$...），可用 `logviewer -hash-password 明文` 生成；
  // 填明文也能用，但启动会打印警告。
  "auth": {
    "username": "admin",
    "password": "",
    "session_ttl_minutes": 720       // 会话有效期（分钟），默认 720=12 小时
  },

  // 机器列表。键名就是界面上显示的别名；"local" 是内置本机，不需要 ssh 字段。
  "hosts": {
    "local": {
      // 本机扫描目录；命令行 -dir 传入的目录会追加到这里并去重。
      "dirs": [
        "./logs"
      ],
      // 每台机器独立的过滤预设（结构与旧 configs.json 一致）。
      "configs": {
        "default_name": "默认配置",
        "configs": {
          "默认配置": {
            "ConfigName": "默认配置",
            "FollowTail": true,
            "ReadLinesLimit": 200,
            "Encoding": "utf-8",
            "CaseSensitive": false,
            "InvertMatch": false,
            "ContextBefore": 0,
            "ContextAfter": 0,
            "UseRegex": true,
            "FilterRule": {
              "TimeStart": "", "TimeEnd": "", "TimePrecision": "second",
              "Levels": ["ERROR", "WARN"],
              "Content": "", "Exclude": "", "CustomRegex": ""
            },
            "HighlightRules": ["ERROR", "WARN"]
          }
        }
      }
    },

    "prod-web-01": {
      "ssh": {
        "host": "10.0.0.11",
        "port": 22,
        "username": "root",
        "password": "changeme",
        // 主机密钥校验。留空则首次连接自动信任(TOFU)并写入该文件；
        // 配好后每次连接严格比对。生产环境不要开 insecure_skip_host_key。
        "known_hosts_file": "",
        "insecure_skip_host_key": false,
        "connect_timeout_seconds": 10,
        "keepalive_seconds": 30
      },
      // 远程平台，留空自动探测；可显式填 linux / darwin / windows。
      "platform": "",
      "dirs": [
        "/var/log/nginx",
        "/opt/app/logs"
      ],
      "configs": {
        "default_name": "默认配置",
        "configs": {}
      }
    }
  }
}
```

### 3.3 目录合并规则

- 启动时把 `-dir` 列表追加到 `hosts.local.dirs`；
- 所有路径经 `filepath.Abs` + `filepath.Clean` 规范化后去重；
- 不存在的目录不报错（保留配置，列目录时返回错误提示），但启动日志会列出哪些目录不可达；
- 远程主机的 `dirs` 不做本机存在性检查，只在第一次浏览时经 SFTP 探测。

---

## 4. 后端架构

### 4.1 模块总览

```
internal/
├── appconfig/        # 新增：logviewer.json 加载(JSONC)、默认模板、旧配置迁移、-hash-password
├── host/             # 新增：Host 抽象 + LocalHost + SSHHost + Manager
│   ├── host.go       #   Host 接口、Manager、共享 Node 定义
│   ├── local.go      #   本机实现（抽离现有 server.go 的 roots/resolve/os.* 逻辑）
│   ├── ssh.go        #   SSH+SFTP 客户端、平台探测、known_hosts TOFU、能力探测
│   └── ssh_proc.go   #   ssh.Session 适配 procmgr.Proc 接口
├── cmdbuild/         # 改造：Build* 加 platform 参数；新增 RemoteCmdLine()
├── procmgr/          # 改造：Start 接收 Proc 接口而非 *exec.Cmd；新增 localProc 包装
├── config/           # 复用：LogConfig 结构；Manager 支持按 host 多实例
└── server/           # 改造：/h/:host 路由前缀、auth 中间件、登录/登出
    ├── auth.go       #   新增
    ├── server.go / dir.go / ws.go / export.go / configapi.go
```

### 4.2 `appconfig` 包

- 用 `github.com/tailscale/hujson` 解析 JSONC（它能容忍注释和尾逗号，再交给 `encoding/json`）。
- 结构定义：

```go
type AppConfig struct {
    Addr  string            `json:"addr"`
    Auth  AuthConfig        `json:"auth"`
    Hosts map[string]HostConfig `json:"hosts"`
}
type AuthConfig struct {
    Username          string `json:"username"`
    Password          string `json:"password"` // 明文或 bcrypt
    SessionTTLMinutes int    `json:"session_ttl_minutes"`
}
type HostConfig struct {
    SSH      *SSHConfig          `json:"ssh"`
    Platform string              `json:"platform"`
    Dirs     []string            `json:"dirs"`
    Configs  config.ConfigStore  `json:"configs"`
}
type SSHConfig struct {
    Host, Username, Password, KnownHostsFile string
    Port, ConnectTimeoutSeconds, KeepAliveSeconds int
    InsecureSkipHostKey bool
}
```

- `Load(path, extraDirs []string)`：定位文件→读取→迁移→合并 `-dir`→返回。
- `GenerateTemplate(path)`：写入 3.2 的内置字符串（用 `embed` 或常量），权限 0600。
- `AuthConfig.Validate(plain string) bool`：密码以 `$2a$`/`$2b$` 开头走 `bcrypt.CompareHashAndPassword`，
  否则用 `subtle.ConstantTimeCompare` 做明文比较。
- 新增 CLI 子命令：`logviewer -hash-password <明文>` 打印 bcrypt 哈希后退出，不启动服务。

### 4.3 `host` 包

#### 接口

```go
type Host interface {
    Name() string
    Platform() (string, error)                 // "linux"|"darwin"|"windows"，探测后缓存
    Dirs() []string                            // 已规范化根目录
    Configs() *config.Manager                  // 每主机独立预设
    ResolvePath(p string) (string, error)      // 按目标平台规则做穿越校验，返回规范化路径
    Ls(dir string) ([]Node, error)             // 单层列目录（目录 + .log/.out）
    Stat(path string) (os.FileInfo, error)
    Open(path string) (io.ReadCloser, error)   // 原始下载：本机 os.Open；远程 sftp.File
    Run(cmd cmdbuild.Command) (procmgr.Proc, error)
}
```

`Node` 从 `internal/server/dir.go` 上浮到 `host` 包，字段保持兼容：
`Name, Path, IsDir, Size, ModTime, HasLog`。

#### LocalHost

- 把现有 `server.go` 的 `roots`、`addRoot`、`resolveAndCheck`、`dir.go` 的 `os.ReadDir`、
  `os.Stat`、`os.Open` 逻辑搬进来，基本不改行为。
- `Platform()` 返回 `runtime.GOOS`。
- `Run(cmd)`：调用 `cmd.BuildCmd()`（本机 `exec.Command`），包成 `localProc` 交给 `procmgr`。

#### SSHHost

**连接管理**

- 持有 `*ssh.Client` + `*sftp.Client`，懒连接，`sync.Once` + mutex 保护。
- `ssh.ClientConfig`：
  - `Auth: []ssh.AuthMethod{ssh.Password(cfg.Password)}`；
  - `HostKeyCallback`：优先 `knownhosts.New(file)`；文件不存在时用 TOFU 回调
    （首次连接把 `hostname:port pubkey` 行追加写入文件后返回 nil）；
    `InsecureSkipHostKey=true` 时才用 `ssh.InsecureIgnoreHostKey()`，并在启动日志打警告。
  - `Timeout`：`ConnectTimeoutSeconds`。
- 保活：连接成功后启 goroutine 每 `KeepAliveSeconds` 发 `ssh.Session.SendRequest("keepalive@openssh.com", ...)`；
  失败则关闭 client 触发下次重连。
- `sftp.Client` 基于同一 `*ssh.Client` 新建；SFTP 子系统不可用时（见漏洞 9）回退 exec 列目录。

**平台探测**（缓存，只做一次）

```
1. exec: uname -s            → Linux→linux, Darwin→darwin
2. 失败则 exec: ver          → 含 "Windows"→windows
3. 都失败 → 返回错误，前端该机器标记为"不可用"
```

探测通过一条 `ssh.Session` 执行；不依赖远程登录 shell，显式用 `sh -c "uname -s"` /
`cmd /c ver`。探测成功后再做能力检查（见 4.6）。

**路径校验 `ResolvePath`**

- Unix 远程：用 `path.Clean`（不是 `filepath.Clean`，因为服务器可能是 Windows），
  对每个配置 root 做前缀 + 分隔符边界校验，等价于现在的 `filepath.Rel` 逻辑但强制 `/`。
- Windows 远程：Go 侧不臆测 8.3 短名/UNC，把路径通过 PowerShell 下发
  `[IO.Path]::GetFullPath($p)` 让远端规范化，再与 root 前缀比对。
- 符号链接：本机用 `filepath.EvalSymlinks`；远程用 `sftp.ReadLink` 循环解析（或 exec
  `realpath`），最终目标若跳出 root 则拒绝。

**列目录 / 下载**

- `Ls`/`Stat`/`Open` 优先走 SFTP（`sftp.ReadDir`、`sftp.Stat`、`sftp.Open`），
  过滤逻辑（目录全展示、文件只显 `.log/.out`、`HasLog` 标记）与本机一致。
- SFTP 不可用时回退：Unix `ls -la --time-style=+%s` 解析、Windows
  `Get-ChildItem | ConvertTo-Json` 解析。

**远程命令执行 `Run`**

- 用 `cmdbuild` 按已探测平台拼好 `Command{Platform, Shell, Script}`；
- 通过 `ssh.Session` 执行：Unix 用 `sh -c '<script>'`，Windows 用
  `<ps> -NoProfile -NonInteractive -EncodedCommand <b64>`（UTF-16LE base64 绕过
  cmd 引号问题；`<ps>` 在连接初始化时探测，远端有 `pwsh` 则用 PowerShell 7+，
  否则回退 `powershell` 5.1）；
- `session.RequestPty("xterm", ...)`：让远程进程有控制终端，关闭会话时 SIGHUP 能送达
  进程组，配合远程命令本身在独立进程组运行，尽量不留孤儿；
- 返回 `sshProc`（实现 `procmgr.Proc`），由 `procmgr` 统一读取/节流/停止。

#### Manager

```go
type Manager struct { hosts map[string]Host }
func (m *Manager) Get(name string) (Host, error)   // 不存在返回错误
func (m *Manager) List() []HostInfo                 // 别名/平台/在线状态，供顶栏切换
```

构造时根据 `AppConfig` 为每个别名建一个 Host；`local` 强制存在（即使配置没写也补一个）。

### 4.4 `cmdbuild` 改造

- 所有导出函数增加 `platform string` 首参：

```go
func BuildView(platform, mode, file, encoding string, limit int, f FilterCfg) Command
func BuildOrigin(platform, file string) Command
func BuildExport(platform, file, encoding string, limit int, f FilterCfg) Command
```

- 函数体把 `runtime.GOOS == "windows"` 改成 `platform == "windows"`。
- `Command` 增加 `Platform string`；`BuildCmd()` 保留（本机用，内部仍按
  `Command.Platform` 选择 `exec.Command("sh"|"powershell", ...)`，不再依赖 `runtime.GOOS`）。
- 新增：

```go
func (c Command) RemoteCmdLine() string {
    if c.Platform == "windows" {
        return `powershell -NoProfile -NonInteractive -Command ` + psQuote(c.Script)
    }
    return `sh -c ` + shQuote(c.Script)
}
```

- 表驱动测试补 platform × mode × encoding 组合，保证三种平台都能拼出预期脚本。

### 4.5 `procmgr` 改造

```go
type Proc interface {
    StdoutPipe() (io.Reader, error)
    StderrPipe() (io.Reader, error)
    Start() error
    Kill() error
    Wait() error
}
```

- `Manager.Start(p Proc, outFn, errFn, doneFn)` 签名替换；读取/flush/Stop 逻辑完全复用。
- 新增 `localProc{cmd *exec.Cmd}` 包装 `*exec.Cmd`，行为与现在一致（`applyProcGroup` 在
  `Start` 前调用）。
- 新增 `sshProc{sess *ssh.Session}`：
  - `StdoutPipe/StderrPipe` 代理到 `sess.StdoutPipe/StderrPipe`；
  - `Start` 调 `sess.Start(cmdLine)`；
  - `Wait` 调 `sess.Wait()`；
  - `Kill`：先 `sess.Signal(ssh.SIGKILL)`，再 `sess.Close()`。
    Unix 上因为请求了 PTY，Close 会让内核发 SIGHUP 到远程进程组；配合命令里
    `tail/grep` 对 SIGHUP 的默认处置，能终止整条管道。
    Windows OpenSSH 对信号支持差，Kill 兜底：另开一个 session 跑
    `Stop-Process -Id <pid> -Force`（PID 由命令启动时通过 `$PID` 输出一行标记回传解析），
    初版若实现成本高，先记录为已知限制并在文档说明。
- `Stop(id)` 仍按「杀→等 done（2 秒超时）」回收，逻辑不变。

### 4.6 远程能力探测

平台探测成功后，异步执行一次能力检查并缓存：

| 能力 | 探测命令 | 缺失影响 |
| --- | --- | --- |
| `tail` | `command -v tail` | 无法跟踪/取末 N 行 |
| `awk` | `command -v awk` | 时间范围过滤不可用 |
| `iconv` | `command -v iconv` | GBK 解码不可用 |
| `grep -E` | `echo x \| grep -E x` | 内容过滤不可用（一般都有） |

Windows 端检查 `powershell` 版本（需 ≥ 3.0）。结果通过 `GET /api/h/:host/capabilities`
暴露给前端，前端据此禁用对应控件并 tooltip 提示「远程主机缺少 iconv，无法 GBK 解码」。

### 4.7 `server` 路由

所有业务路由加机器前缀，登录与静态资源除外：

```
POST /api/login
POST /api/logout
GET  /api/hosts                     # 列出所有机器：别名/平台/在线/能力

GET  /api/h/:host/dir/roots
GET  /api/h/:host/dir/list?path=
GET  /api/h/:host/capabilities

GET  /api/h/:host/config/list
GET  /api/h/:host/config/get?name=
POST /api/h/:host/config/save
POST /api/h/:host/config/delete
POST /api/h/:host/config/rename
POST /api/h/:host/config/setdefault
POST /api/h/:host/config/preview

GET  /api/h/:host/file/download/origin?path=
POST /api/h/:host/file/download/filter?path=

GET  /ws?host=:host
```

- 每个 handler 第一步 `s.hosts.Get(host)`，拿到 Host 后路径校验/列目录/开文件/跑命令都走
  Host 接口，server 层不再出现 `runtime.GOOS`/`os.*`/`exec.*`。
- 原始导出：本机走 `BuildOrigin`+`streamCommandToResponse`（保留 Content-Length）；
  远程用 `host.Open` + `io.Copy`，`Stat` 得到的大小写 Content-Length。
- 过滤导出：本机与远程统一 `host.Run(cmdbuild.BuildExport(...))`，stdout 流式写响应。
- 后端访问日志：现有 `log.Printf("[ws] 查看命令 ...")` 会打全路径。远程场景下改为默认
  只打 `host=alias file=basename shell=sh`，加 `-verbose` 才打印完整脚本和绝对路径
  （路径可能含敏感信息）。

### 4.8 认证与会话（`auth.go`）

- 启用条件：`auth.username != ""`。未启用时所有中间件直接放行（行为同现在）。
- 登录：
  - `POST /api/login`，JSON `{username,password}`；
  - 失败统一 `time.Sleep(1s)` 返回 401；内存维护 IP→失败计数，5 次/分钟锁定 5 分钟；
  - 成功生成 32 字节随机 token（`crypto/rand`），存 `map[token]time.Time`（过期时间），
    Set-Cookie：
    `lv_sess=<token>; HttpOnly; SameSite=Lax; Path=/; Max-Age=<ttl>`，
    若请求是 HTTPS 或 Host 不是 localhost/127.0.0.1 再加 `Secure`。
- 登出：`POST /api/logout` 从 map 删 token 并清 cookie。
- 中间件：
  - `/api/login`、静态资源（`/`、`/static/*`）放行；
  - 其它 `/api/**` 校验 cookie token，无效返回 401；
  - WebSocket 在 Upgrade 前同样校验 cookie。
- 会话滑动续期：每次通过校验把过期时间重置为 `now+ttl`。
- 重启进程后所有会话失效（无持久化需求；可接受）。
- WebSocket `CheckOrigin`：启用认证时，校验 `Origin` 的 host 与请求 `Host` 同源，
  不同源拒绝 Upgrade（防跨站 WS 劫持）；未启用认证时维持 true 并打警告。

---

## 5. 前端设计

### 5.1 登录页

- 新增 `static/login.html`（极简：用户名、密码、登录按钮、错误提示）。
- 入口逻辑：`app.js` 启动时若任意 API 返回 401，跳转 `/login.html?next=<当前路径>`；
  登录成功后跳回 `next`。
- 为保持单二进制 embed，`login.html` 放进 `static/` 一起打包。

### 5.2 机器切换

- 顶栏 `#rootSelect` 前新增 `#hostSelect`：
  - 选项来自 `GET /api/hosts`，文案「别名 (平台)」，前面一个状态圆点（在线/离线/探测中）；
  - 默认选 `local`；选择持久化到 `localStorage`。
- 切换动作：
  1. 发 `stop` 并关闭当前 WS；
  2. 清空目录树、清空终端、清空 `#rootSelect`；
  3. 拉 `/api/h/<host>/dir/roots` 填充 rootSelect；
  4. 拉 `/api/h/<host>/capabilities`，更新编码/时间过滤控件可用性；
  5. 以 `wss?host=<host>` 重连 WS。
- 后续所有目录/配置/导出请求都带 `/h/<host>/` 前缀；当前 host 存在全局变量 `currentHost`。

### 5.3 可拖拽侧栏

- `index.html` 在 `#treePanel` 与 `#content` 之间插入：

```html
<div id="splitter" title="拖拽调整宽度"></div>
```

- `style.css`：
  - `#treePanel` 宽度从固定 `260px` 改为 CSS 变量 `--tree-w: 260px`；
  - `#splitter` 宽 4px、`cursor: col-resize`、hover 高亮；拖拽中 `body.user-select:none`；
  - 树节点 `.tree-item` 加 `white-space:nowrap; overflow:hidden; text-overflow:ellipsis`，
    并给节点 DOM 加 `title=完整路径`。
- `app.js`：
  - mousedown 在 splitter 上 → 开始拖拽，mousemove 计算新宽度
    `clamp(startX + delta, 180, 600)`，写 `document.documentElement.style.setProperty('--tree-w', w+'px')`；
  - mouseup 把最终宽度写 `localStorage('treeWidth')`；初始化时读取恢复。
  - 收起/展开按钮逻辑保留，collapsed 时隐藏 splitter。

### 5.4 WebSocket 协议变化

- 连接地址从 `/ws` 改为 `/ws?host=<alias>`；
- 上行 `start` 消息里的 `filePath` 是目标机器上的路径（不再做本机 cwd 拼接）；
- 下行不变：`log`/`error`/`status`。
- 新增下行 `status: "host-unavailable"`：SSH 连接/平台探测失败时，前端在该机器名下
  显示错误并禁止「开始跟踪」。

---

## 6. 漏洞与风险评审

设计阶段识别的问题，以及对应措施。**这是本文档重点，请评审时逐条确认。**

### 6.1 凭证明文存储（中危）

- `logviewer.json` 含 SSH 密码和登录密码。
- 措施：生成时权限 `0600`；登录密码提供 `-hash-password` 工具并在启动时对明文密码打警告；
  文档明确「该文件的读权限等同于所有被纳管机器的登录权限」。SSH 私钥/agent 认证留待后续。

### 6.2 SSH 主机密钥伪造（MITM，高危）

- 绝不能默认 `InsecureIgnoreHostKey`。
- 措施：默认 TOFU——首次把主机公钥追加到 `known_hosts_file`（默认 `<exe>/known_hosts`），
  之后严格校验；`insecure_skip_host_key` 仅用于测试，开启时启动日志打红字警告。

### 6.3 远程命令注入（高危）

- 路径、过滤模式都来自前端，拼进远程 shell。
- 措施：继续使用 `shQuote`/`psQuote` 参数化引用；远程**强制**通过 `sh -c` /
  `powershell -NoProfile -Command` 执行，不依赖用户登录 shell（避免 zsh/csh/fish
  引号规则差异）；自定义正则框只在用户明确勾选「正则」时原样传入，UI 提示风险。

### 6.4 远程路径穿越（高危）

- 路径是远程机器的，不能用本机 `filepath` 规范化；Windows 还涉及盘符、UNC、8.3 短名。
- 措施：Unix 远程用 `path.Clean`+边界前缀校验；Windows 远程下发 `[IO.Path]::GetFullPath`
  让远端规范化后再比 root；符号链接用 `EvalSymlinks`/`realpath` 解析最终目标，越界即拒。
  为每个 host 独立维护 roots，不能跨机器拼接路径。

### 6.5 认证与会话安全（中危）

- Cookie 被截获即可冒充；无 HTTPS 时密码明文传输。
- 措施：`HttpOnly` + `SameSite=Lax`，HTTPS 下加 `Secure`；登录失败限流防暴力破解；
  WebSocket 启用 auth 后做同源 Origin 校验；绑定非 loopback 且无 TLS 时启动警告，
  并要求显式 `-insecure-allow-remote` 才允许裸奔。文档建议生产环境走反代 HTTPS。

### 6.6 远程进程残留（中危）

- 远程 `tail -F | grep | awk` 是多个进程，SSH 会话关闭不一定能杀干净，Windows 尤甚。
- 措施：Unix 请 PTY + 独立进程组 + SIGKILL；Windows 用 `Stop-Process` 按 PID 兜底；
  文档列已知限制；后续可在远程注入一个轻量 watchdog，但本期不做。

### 6.7 远程能力缺失（低危）

- 最小化容器可能没有 `iconv`/`awk`。
- 措施：连接时探测能力并缓存，前端禁用对应控件；不要让命令静默失败后用户对着空白终端发呆。

### 6.8 SFTP 子系统被禁用（低危）

- 部分加固的 sshd 关掉了 sftp 子系统。
- 措施：`Ls/Stat/Open` 在 SFTP 失败时回退到 exec（`ls -la`/`Get-ChildItem | ConvertTo-Json`、
  `cat`），能力探测结果里标记 `sftp:false`。

### 6.9 日志泄露敏感路径（低危）

- 现有 `log.Printf` 打全量命令脚本，远程场景下可能把敏感目录路径打到控制台/日志文件。
- 措施：默认只打 `host + basename + shell`，`-verbose` 才打全量。

### 6.10 误暴露到公网（高危）

- 加了 SSH 能力后，一旦 LogViewer 本身被攻破，等于拿到所有纳管机器的凭证。
- 措施：默认 `127.0.0.1`；绑定 `0.0.0.0` 且无 auth 时拒绝启动（除非显式
  `-insecure-allow-remote`）；安全说明置顶写在 README。

### 6.11 现有代码已存在但之前未记录的问题

- 本机路径校验未解析符号链接：`root` 内若有指向 `/etc` 的软链，现有 `filepath.Rel` 会放行。
  本次重构 `LocalHost.ResolvePath` 一并加 `filepath.EvalSymlinks`。
- WebSocket `CheckOrigin: func(*http.Request) bool { return true }` 允许任意网页跨站
  建立 WS（虽然本地工具风险低，但开了认证后必须收紧）。

---

## 7. 实施阶段建议

改动较大，建议分四个 PR 逐步合并，每步都保持可编译可运行：

1. **阶段一：配置与 Host 抽象（不接 SSH）**
   - 引入 `appconfig`、`host` 接口与 `LocalHost`，把现有 server 逻辑迁过去；
   - `cmdbuild` 加 platform 参数；`procmgr` 改 Proc 接口；
   - 路由加 `/h/:host` 前缀（此时只有 `local`）；
   - 旧 `configs.json` 迁移；
   - 保证本机行为完全不变（回归测试）。
2. **阶段二：SSHHost 基础能力**
   - SSH+SFTP 连接、平台探测、known_hosts TOFU、列目录、原始下载；
   - 前端机器切换器；
   - 暂不支持远程过滤导出（先只看远程文件原始内容或静态读取）。
3. **阶段三：远程命令管道**
   - `sshProc` 适配 `procmgr.Proc`；远程跟踪/过滤/GBK/时间范围；
   - 能力探测与前端联动；进程残留兜底；
   - 过滤导出远程化。
4. **阶段四：登录认证 + UI 打磨**
   - `auth.go`、登录页、会话/cookie/限流、WS Origin 校验；
   - 可拖拽侧栏、长文件名省略号；
   - 文档更新、安全自检。

---

## 8. 关键文件清单

| 文件 | 改动类型 |
| --- | --- |
| `main.go` | 改：加载 AppConfig、`-config`/`-hash-password`/`-insecure-allow-remote`/`-verbose` |
| `internal/appconfig/appconfig.go` | 新增 |
| `internal/host/{host,local,ssh,ssh_proc}.go` | 新增 |
| `internal/cmdbuild/cmdbuild.go` | 改：platform 参数 + `RemoteCmdLine()` |
| `internal/cmdbuild/filter_test.go` | 改：补 platform 用例 |
| `internal/procmgr/procmgr.go` | 改：`Proc` 接口 + `localProc` |
| `internal/procmgr/procsys_*.go` | 不变（localProc 内调用） |
| `internal/config/config.go` | 小改：Manager 支持多实例（已无全局状态，基本不动） |
| `internal/server/{server,dir,ws,export,configapi}.go` | 改：`/h/:host` 前缀 + Host 接口 |
| `internal/server/auth.go` | 新增 |
| `static/login.html` | 新增 |
| `static/index.html` | 改：hostSelect、splitter |
| `static/app.js` | 改：机器切换、登录跳转、拖拽 |
| `static/style.css` | 改：`--tree-w`、splitter、节点省略号 |
| `go.mod` | 加 `golang.org/x/crypto/ssh`、`github.com/pkg/sftp`、`golang.org/x/crypto/bcrypt`、JSONC 库 |
| `README.md`、`docs/*.md` | 改：修正过时项 + 补 SSH/认证说明 |

---

## 9. 验证清单

1. **本机回归**：`go run . -dir ./config`，目录浏览/跟踪/过滤/GBK/导出/配置 CRUD 全部正常。
2. **配置生成与查找**：删除 logviewer.json 启动，确认 exe 目录生成带注释模板、权限 0600；
   在 cwd 放另一份验证 exe 优先；`-config` 能覆盖。
3. **旧配置迁移**：保留旧 `config/configs.json` 启动，确认 `hosts.local.configs` 继承预设、
   旧文件备份为 `.bak`。
4. **SSH Linux**：自动探测 platform、SFTP 列目录、`tail -F` 跟踪、GBK(iconv)、时间过滤(awk)、
   停止后 `ps aux | grep -E 'tail|grep|awk'` 无残留。
5. **SSH macOS**：BSD tail/awk 下跟踪与时间过滤正常。
6. **SSH Windows**：OpenSSH Server + PowerShell，`Get-Content -Wait`/`Select-String`/停止。
7. **路径穿越**：`?path=/etc/passwd`、`../../etc/passwd`、Windows 8.3 短名、UNC、软链越界
   均被拒。
8. **known_hosts**：首次连接自动写入；篡改 known_hosts 后连接被拒；`insecure_skip_host_key`
   开启时日志有警告。
9. **认证**：未登录访问 `/api/**` 401 跳登录；错误密码限流；cookie 带 HttpOnly/SameSite；
   WS 跨 Origin 被拒；登出后旧 token 失效；TTL 过期重新登录。
10. **UI**：机器切换、侧栏在 180–600px 间拖拽、刷新记忆宽度、长文件名省略号 + tooltip。
11. **安全启动**：绑定非本机地址且无 auth 时**启动打印醒目警告**（设计稿原拟"拒绝启动 +
    `-insecure-allow-remote` 放行"，实现改为只警告不拦截，便于内网临时使用；见下）。
12. `go build ./...`、`go test ./...` 通过。

---

## 10. 实现偏差（实际实现 vs 本设计稿）

- **登录开关**：设计稿用"`username` 留空表示不启用"，实现改为显式 `auth.enabled` 布尔值
  （默认 `false`，完全不做登录校验），满足"需要一个配置项彻底关闭登录"的诉求。
  `enabled=true` 且 `username` 非空才启用认证。
- **登录 UI**：未单独建 `login.html`，而是在主页加居中登录遮罩（`#loginMask`）；
  未登录访问受保护接口返回 401，前端拦截后弹出遮罩，不整页跳转。
- **远程 Windows 命令**：设计稿用 `powershell -Command`，实现改为 `-EncodedCommand`
  （UTF-16LE base64）彻底绕过引号转义；停止时用 `taskkill /T /F` 杀进程树。
  脚本里额外加 `$ProgressPreference='SilentlyContinue'`，避免 PowerShell 把
  "正在准备首次使用模块"等进度以 CLIXML 写到 stderr 污染前端错误提示；PID 标记后显式
  `[Console]::Error.Flush()`，确保标记能被及时读到以精确查杀。
- **停止性能**：Unix 远程停止从 ~435ms 优化到 ~25ms（直接 `kill -KILL` 进程组，去掉
  `sleep 0.2`，先关 SSH 会话让管道立即 EOF，再异步兜底查杀）；Windows OpenSSH 受其通道
  关闭机制限制约 ~1s，前端停止按钮立即进入非阻塞"停止中"loading 态。
- **进程回收等待**：`procmgr.Stop` 等待 EOF 的上限由 2s 调为 1s。
- **优雅关闭**：用 `http.Server.Shutdown` + `signal.NotifyContext`，`Server.Close()`
  先 `procmgr.StopAll()` 再 `host.Manager.Close()`（关 SSH）。
- **静态资源缓存**：`/static/**` 与 `/` 响应加 `Cache-Control: no-cache`，避免发版后
  浏览器沿用旧 `app.js` 导致"修了却还复现"。
- **追踪空白行**：前端 `writeToTerminal` 原先对每个以 `\n` 结尾的批次额外补一个换行，
  导致 follow 新日志到达时多一个空行；已改为按 `\n` 切分、剥离 `\r` 后只在行间输出
  `\r\n`，不再追加尾随换行（local/win-local 实测数据一致）。
- **Windows 平台探测命令**：设计稿用 `cmd /c ver`，实际改为裸 `ver`——Win32-OpenSSH
  默认 shell 已是 cmd.exe，再嵌套 `cmd /c ver` 会触发 cmd 引号处理 bug（报
  `'ver"' 不是内部或外部命令`，真实 Windows OpenSSH 上复现）；默认 shell 被改成
  PowerShell 时再以 PowerShell 兜底。
- **known_hosts 默认路径**：设计稿写"默认 `<exe>/known_hosts`"，实际为空时使用
  `~/.ssh/known_hosts`（用户主目录），更符合 SSH 习惯。
- **配置热加载**：设计稿（3.1）标注"不在本期范围"，后续已实现：前端"重载配置"按钮
  或 Unix `SIGHUP` 触发热加载，未变更的 SSH 主机会话保留，并通过 WS `reconnect`
  指令通知对应连接迁移到新 Host 实例。
- **CLI 参数**：`-insecure-allow-remote` / `-verbose` 未实现（非本机绑定无认证时
  只打印警告，不拒绝启动）；新增了 `-key` / `-encrypt-config` / `-decrypt-config`
  用于 AES-256-GCM 配置密码加解密。
