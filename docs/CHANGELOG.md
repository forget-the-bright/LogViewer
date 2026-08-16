# 变更日志

本项目版本号唯一来源是根目录 `VERSION` 文件，**只能由开发者本人手动修改**，
构建时通过 `-ldflags "-X main.version=..."` 注入。格式参考
[Keep a Changelog](https://keepachangelog.com/zh-CN/)。

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
