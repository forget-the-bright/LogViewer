# 变更日志

本项目版本号唯一来源是根目录 `VERSION` 文件，**只能由开发者本人手动修改**，
构建时通过 `-ldflags "-X main.version=..."` 注入。格式参考
[Keep a Changelog](https://keepachangelog.com/zh-CN/)。

## [v0.0.6] - 2026-08-17

一次可靠性与正确性加固版本：修复上一轮编码改造引入的 Windows GBK 性能回退，
补齐时间过滤/认证/会话/导出等环节的边界与竞态问题，新增开发期命令日志开关。

### 新增
- **`log_commands` 开发模式配置**：设为 `true` 时把每条发往目标机器的查询/导出/
  正则校验命令（shell、platform、完整脚本）以 INFO 级别打印到服务端日志，用于排查
  命令构造与性能问题。支持热加载即时生效；生产环境不建议开启（follow 会持续输出，
  且命令可能包含文件路径）。

### 修复
- **P0 Windows GBK 查询性能回退**：上一版编码改造在所有 Windows GBK 路径上引入了
  逐行 `ForEach-Object` 转码或 `ReadLines` 全量枚举，中文机（代码页 936，转码本是
  恒等映射）也照跑，静态/跟踪、带不带过滤都明显变慢。改为**运行时代码页分流**
  （`[Text.Encoding]::Default.CodePage -eq 936`）：中文系统走纯原生
  `Get-Content -Encoding Default -Wait/-Tail`，与旧版性能一致（实测 ~51ms，对比
  逐行转码 ~124ms、ReadLines ~617ms）；非中文系统才在 else 分支逐行用
  `GetEncoding('GBK')` 转码。`-Encoding Default` 取代了区域相关、英文系统上会
  乱码的 `-Encoding OEM`。
- **时间过滤非法输入静默退化为"读取全部"**：`TimeBounds` 签名由 `(start,end,ok bool)`
  改为 `(start,end,err error)`，无法解析的时间串、终点早于起点一律返回错误；
  查看/导出/预览三处调用方在写响应头/启动命令前把错误返回给用户，不再悄悄返回全量日志。
- **认证热更新数据竞争**：`Enabled()`/`Username()`/`Login()` 此前无锁读取
  `enabled/username/check/ttl`，与 `UpdateAuth` 的热更新构成数据竞争（`go test -race`
  可复现）。`Login()` 在锁内快照校验所需字段，新增并发安全的 `TTL()`；
  `tooManyFailures` 在某 IP 的失败记录全部过期时从 map 删除，避免随爆破 IP 无限增长。
- **断线补齐日志乱序**：`viewSession.attach` 原先先取 `mu` 再取 `writeMu`，两次加锁
  之间实时 `onStdout` 可能先写入一帧，导致补发帧（seq 较小）排在实时帧之后。
  调整锁序为先 `writeMu` 后 `mu`，整个补发期间阻塞实时写入，保证补发先于实时到达。
- **WS ping goroutine 泄漏**：连接断开后 ping goroutine 要等下一个 30s tick 写入失败
  才退出。新增 `pingDone` channel，读循环返回时立即关闭，goroutine 即时退出。
- **导出失败仍带着下载头**：原始/过滤导出此前先写 `Content-Disposition`/`Content-Type`
  再打开文件或启动命令，一旦 Open/Run 失败（权限变更、SSH 中断），错误 JSON 会带着
  下载头返回，浏览器下载行为异常。改为先打开/启动成功再写下载头，失败干净返回 JSON。
- **保存预设不校验数值边界**：越界的读取行数/上下文行数能被"成功"保存，直到查看/导出
  才报错，预设形同损坏。`handleConfigSave` 在写盘前调用 `cfg.Validate()`。
- **SSH 远程命令 PID 解析/退出日志**：`pidFilterReader` 改为扫描前若干行 banner 再定位
  `LV_PID=` 标记（兼容登录脚本输出的 banner），banner 行原样透传；`Wait()` 对非主动
  杀死的非零退出记录 `slog.Warn`，不再静默吞掉。
- **前端**：
  - 集中 `resetRunState()` 清理 `sessionID/lastSeq`，修复切换主机/文件/模式、停止、
    断线复位时残留旧会话标识导致重连错误 attach 到已销毁会话的问题。
  - 高亮规则/正则/大小写标志改为随本次查看会话的配置快照传入（`highlightContext()`），
    不再逐行读实时 DOM，修复 follow 输出期间用户改复选框导致同屏各行高亮错配。
  - 切换主机引入 `hostSwitchGen` 代次令牌，快速连切两台主机时旧的异步加载不会再用
    旧主机配置覆盖新主机表单。
  - 连接已断时点停止不再永久卡在"停止中"：`wsSend` 失败时本地复位运行态。
  - 导出文件名优先解析 RFC 5987 `filename*=UTF-8''...`，修复中文名下载乱码；
    原始导出加在途锁防重复触发。
  - 重命名预设后显式选中新名；切换能力清空时间选择器后同步刷新预览；复制按钮对未初始
    化终端做空值守卫；移除无用的 `triggerDownload`/`timeInputs`。

### 变更
- 正则校验函数 `validateFilter`/`runRegexCheck` 改为 `Server` 方法，以便统一记录命令日志。
- `viewSession` 移除恒为 true 的冗余 `follow` 字段。

## [v0.0.5] - 2026-08-17

### 新增
- **断线缓冲补齐**：follow 会话与 WebSocket 连接解耦，断线宽限期（`session_grace_seconds`，
  默认 45s）内进程持续运行、输出进入 2MB 有界环形缓冲；重连发 `attach` 按 `seq` 自动补发缺口，
  缓冲溢出给出 `gap` 提示。静态模式仍连接绑定。
- **可观测性**：`GET /healthz` 返回各主机连通状态；`GET /metrics` 暴露 Prometheus 指标
  （活跃 WS 连接、日志进程数、SSH 重连次数、导出/下发字节数）；新增 `log_json` / `log_level`
  配置输出结构化 JSON 日志（`internal/applog` 统一 slog 并重定向标准库 log）。
- **界面体验**：主题跟随系统（自动/明/暗三态）、中/英国际化、紧凑/舒适密度、命令面板
  （`Ctrl+Shift+P`）、完整快捷键体系与 `?` 帮助、滚动到顶/底悬浮按钮、移动端响应式
  （汉堡菜单/全屏侧栏/底部配置抽屉）；偏好统一持久化。
- **顶栏帮助/控制面板按钮**：右上角新增 `?`（快捷键帮助）与 `⌘`（命令面板）两个独立弹窗按钮。
- **搜索结果计数**：Ctrl+F 终端搜索实时显示【当前 / 总数】（如 `3 / 127`），上下切换时同步更新；
  超过装饰上限（5000）如实显示 `5000+`，无匹配显示"无匹配"。数据来自 search addon 官方
  `onDidChangeResults` 事件，非自行计数。
- **可配置 scrollback**：设置抽屉提供 5k/10k/20k/50k 档位（默认 10k）。
- **轮转/截断提示**：`tail -F` 检测到日志轮转或截断时显示可关闭的非红色提示条，不再误报为错误；
  "文件出现"等良性噪声静默。
- 浏览器渲染压测页 `static/bench.html`（dev-only）与服务端管道压测 `BenchmarkReadLoopThroughput`
  （20 万行约 25ms 排空，~8M 行/s）。

### 修复
- **P0 Ctrl+F 在日志区失效 / Ctrl+C 无法复制**：根因为 xterm 在捕获阶段对这些组合键
  `preventDefault`。改用官方 `attachCustomKeyEventHandler` 对浏览器/UI 快捷键返回 `false`
  （不阻止冒泡与默认行为），从根修复而非临时屏蔽事件。
- **终端区 g/G/PgUp/PgDn 等快捷键失效**：根因有二——① `isTyping()` 把 xterm 聚焦时持有的只读
  隐藏 textarea（`.xterm-helper-textarea`）误判为"正在输入"；② xterm 在捕获阶段对这些键
  `stopPropagation+preventDefault`。修复：守卫排除该辅助 textarea，并在 custom key handler 中
  对无修饰键的全局快捷键返回 `false` 放行，事件正常冒泡到全局处理器；方向键等仍交 xterm。
- `TestValidate` 补齐新增 `LogLevel`/`SessionGraceSeconds` 校验所需字段，并新增非法值用例。

### 变更
- SSH `ssh.Client`/`sftp.Client` 单例复用经核实并加回归测试（连续 Ls/Stat/Open 只产生 1 个
  TCP 连接）；重连经 `onReconnect` 回调计入 `logviewer_ssh_reconnects_total`。

## [v0.0.4] - 2026-08-16

### 新增
- **配置密码加密**：AES-256-GCM 加密 `logviewer.json` 中的 SSH/登录密码（`enc:v1:` 前缀），
  密钥通过 `-key` 或 `LOGVIEWER_KEY` 提供；新增 `-encrypt-config` / `-decrypt-config`
  一次性加解密命令；bcrypt 哈希保持不变。
- **配置热加载**：前端"重载配置"按钮或 Unix `SIGHUP` 运行时重新加载配置；按 SSH 连接指纹
  判断主机是否变更，未变更的会话与正在跟踪的日志不中断；变更的主机通过 WS `reconnect`
  指令通知连接迁移，认证配置热更新。
- **JSONC 注释保留**：通过 hujson AST 局部补丁（JSON Pointer 定位 `configs` 子树）写回
  过滤预设，不再全量序列化剥光注释。
- **断线自动重连**：WS 断线指数退避重连（1s→30s），跟踪中断线保留 `pendingResume`，
  重连后自动续跟；握手失败（401）探测 `/api/auth/status`，会话过期弹登录框而非无限重连。
- **版本管理**：新增根目录 `VERSION` 文件作为版本号唯一来源，`build_all_platforms.ps1`
  与 `go build` 均从中读取，启动日志打印版本号。
- **导出取消**：导出遮罩新增"取消导出"按钮（AbortController，服务端随客户端断开查杀进程）；
  机器列表刷新加在途锁去重。
- 配置预设新增"重命名"按钮；编码下拉补 GB2312；非安全上下文剪贴板兜底。

### 修复（可靠性加固 P0–P2）
- **行号栏滚动冻结**：改用 xterm 缓冲区绝对坐标（`baseY + length`）计算新增行，修复
  scrollback 封顶后行号永久冻结。
- **SSH 进程 Start 失败会话泄漏**：`procmgr.Start` 在 `Start()` 返回错误时对称 `Kill()`。
- **单边时间范围过滤**：`TimeBounds` 支持只填开始/只填结束。
- **WS 异步回调竞态**：引入连接代次 `wsGen` 守卫，旧连接回调不再污染新连接。
- **暂停状态复位**：统一 `resetPauseState()`，切换文件/主机、断线、停止时清零暂停缓冲。
- **离线点开始无反馈**：未连接时点开始明确提示并主动发起连接。
- **Ctrl+F 终端搜索无效**：开启 `allowProposedApi`，使 search addon 高亮装饰生效。
- **local-web-02 平台探测失败**：Win32-OpenSSH 默认 shell 为 cmd.exe，嵌套 `cmd /c ver`
  触发引号 bug；改为裸 `ver` + PowerShell 兜底。
- **SSH 在途操作误杀新连接**：新增 `invalidateConn(old)` 比较拆除，修复并发在途操作中一个
  失败会关闭另一个刚建好的新连接的竞态。
- **"开始跟踪"按钮无法点击**：WS 连接建立后未刷新按钮态，导致空闲时按钮永久禁用，补上
  `updateButtons()`。
- **左侧菜单栏无法折叠**：分隔条拖拽写入的内联 `width` 覆盖了 `.collapsed{width:0}` 样式，
  折叠时显式置零内联宽度。
- 后端参数范围校验（读取行数 ≤100 万、上下文 ≤1 万、端口/超时范围）；配置按钮统一异常
  捕获；主机名属性选择器改为按值查找防注入；目录展开加在途锁。

### 文档
- 全局文档治理：修正过时描述（平台探测命令、known_hosts 默认路径、gutter 坐标、WS 消息、
  构建命令等），统一版本号说明，新增本 CHANGELOG，删除与 lessons.md 重复的临时追踪文档。

## [v0.0.1] - 2026-08-13

### 新增
- 初始版本：跨平台本地日志查看，Go（Gin + WebSocket）后端 + xterm.js 前端，单二进制自包含。
- "一切都是命令"架构：Unix `tail/cat/grep/awk/iconv`、Windows PowerShell 原生命令管道。
- 实时跟踪 / 静态查看、过滤规则构建器（时间/级别/内容/排除/上下文）、GBK 编码、行号、
  高亮、配置预设、流式导出、明暗主题。
- 修复 Linux 时间/正则过滤不生效（awk `{n}` 量词与 ERE 非捕获组方言问题）与跟踪无历史问题。
