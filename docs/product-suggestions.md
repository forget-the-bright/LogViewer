# 产品体验建议清单

本文件按优先级和实现成本梳理 LogViewer 后续可补充的能力，供规划参考。
标记说明：★ 低优先级 / ★★ 中等 / ★★★ 高价值且用户感知明显。

## 一、连接与主机管理

- **★★ SSH 私钥认证**：当前仅支持密码。增加 `private_key_file` / `private_key_passphrase`
  字段，支持 PEM/OpenSSH 格式 ed25519/rsa 密钥，配合 ssh-agent 转发。
- **★ 连接配置测试按钮**：在顶栏或配置页加"测试连接"，实时返回 SSH 握手/平台探测/目录可访问性结果，
  避免保存配置后才发现连不上。
- **★ 主机分组/标签**：机器数量多时按环境（prod/staging）、业务线分组折叠。
- **★★ 批量操作**：多台机器同一日志路径同时查看，或一次导出多机过滤结果。
- **★ 跳板机/ProxyJump**：支持 `ssh -J` 语义的多级跳转。

## 二、日志查看增强

- **★★★ 多标签页**：同时打开多个日志文件（不同主机/路径），标签页间快速切换，
  每个标签独立的终端与过滤状态。
- **★★★ 终端内搜索**：Ctrl+F 在当前终端缓冲区内搜索并高亮（xterm.js 自带 addon 可复用）。✅ 已完成（修复 `allowProposedApi` 使 search addon 装饰高亮生效）。
- **★★ 大文件分段加载**：静态模式下对超大文件支持按时间/行号范围加载，而非一次性 `tail -n`。
- **★★ 日志统计概览**：侧边栏或悬浮面板展示 ERROR/WARN/INFO 计数趋势图（按分钟/小时聚合），
  点击柱状图跳转到对应时间段。
- **★ 书签/收藏**：常用文件路径加星标置顶，跨主机全局收藏夹。
- **★ 时间轴缩略图**：终端顶部显示日志密度/错误分布迷你图，点击跳转。
- **★ 自定义高亮规则持久化**：每台机器可保存多套高亮配色方案。

## 三、告警与协作

- **★★ 实时告警规则**：配置关键词/正则规则，命中时浏览器桌面通知或声音提醒，
  可选 webhook 推送到飞书/钉钉/Slack。
- **★ 审计日志**：记录谁在什么时候查看/导出了哪个文件（多用户场景有合规价值）。
- **★ 分享链接**：生成带时效的只读链接（含主机、路径、过滤参数），他人打开可直接看到同样视图。

## 四、配置与部署

- **★ 配置导入/导出**：整包导出 logviewer.json（脱敏或带密钥），或单独导入某台主机配置。
- **★ 配置 Web 编辑器**：在界面里增删主机、编辑 SSH 参数、测试连接，而不必手改 JSON。
- **★★ 多用户与权限**：当前是单用户模型。多用户场景下按主机/目录授权，配合反向代理 SSO。
- **★ 配置 schema 校验**：编辑时给出字段错误提示（已有 Validate，可暴露到 UI）。

## 五、界面与体验

- ✅ **暗色/亮色主题跟随系统**：`prefers-color-scheme` 检测 + 自动/白天/黑夜三态，手动切换后停止跟随，
  持久化到 `logviewer-prefs`，xterm 配色同步。
- ✅ **移动端响应式适配**：`@media (max-width:768px)` 汉堡菜单、侧栏全屏覆盖、配置面板底部抽屉。
- ✅ **键盘快捷键体系**：统一 `initShortcuts()`，`/ t s g G PgUp/PgDn ? Ctrl+Shift+P` 全量接线，
  输入框焦点守卫（`isTyping()`），`?` 快捷键浮层；`[`/`]` 预留给多标签页。
- ✅ **国际化**：新增 `static/i18n.js`（中/英），静态 DOM 用 `data-i18n`、动态串用 `t()`；
  `i18n.test.mjs` 校验两套键集合一致，硬编码中文已全部迁移。
- ✅ **紧凑/舒适密度**：字号（12–18）、行高（1.0–1.6）在设置抽屉调节并持久化。
- ✅ **命令面板**：`Ctrl+Shift+P` 唤起，子序列模糊匹配，覆盖开始/停止/清空/导出/切主题/跳转等全部操作。
- ✅ **滚动悬浮按钮**：依据 `viewportY/baseY/length` 智能显隐的回到顶/底按钮，上滚时"到底部"兼作继续跟踪。
- ✅ **P0 全局快捷键根因修复**：xterm 在**捕获阶段** `preventDefault` 杀死 Ctrl+F；Ctrl+C 被解析为 ETX
  导致浏览器原生 copy 不触发、xterm 的 `copyHandler` 不执行。通过 `attachCustomKeyEventHandler` 对
  这些组合键返回 `false`（不阻止冒泡/默认行为）根治，而非临时屏蔽事件。

## 六、可靠性与性能

- ✅ **断线状态下保留缓冲**：follow 会话与 WS 连接解耦（`viewSession`），进程持续运行，输出写入 2MB 有界
  环形缓冲并分配单调 `seq`；断线进入宽限期（`session_grace_seconds`，默认 45s），重连发 `attach` 按
  `lastSeq` 补发缺口；`lastSeq < oldestSeq`（已淘汰）时下发 `gap` 通知；`writeMu` 保证补发先于实时帧。
  静态模式仍连接绑定（断了重来，无补齐意义）。
- ✅ **前端虚拟滚动评估**：经核实 xterm.js 自身已对 viewport 做 canvas/DOM 复用的虚拟化渲染，真正的杠杆是
  `scrollback` 上限与写入批量化，而非重写渲染层。`scrollback` 已可配置（设置抽屉 5k/10k/20k/50k，默认 10k，
  持久化，下次启动生效）；写入仍由 procmgr 40ms/512 行批量保证。服务端管道压测 20 万行约 25ms 排空
  （~8M 行/s，见 `BenchmarkReadLoopThroughput`）；浏览器渲染压测页 `static/bench.html` 可实测耗时/堆内存。
- ✅ **日志文件轮转体验**：`classifyStderr` 区分 `tail -F` 的"文件出现/建立"（良性忽略）、"被替换/轮转"
  （notice rotate）、"文件截断"（notice truncate）与真实错误；前端显示可关闭的非红色提示条。
- ✅ **SSH 连接池（核实）**：`ssh.Client` 与 `sftp.Client` 已是单例复用（`ensureConnected`/`withSFTP`），
  每条远程命令的 session 因需独立 stdio/信号天然每次新建；重连走 `invalidateConn(old)` 比较拆除避免误杀新连接，
  并通过 `onReconnect` 回调累加 `logviewer_ssh_reconnects_total`。新增测试断言连续 Ls/Stat/Open 只产生 1 个
  TCP 连接（`TestSSHIntegration_LsStatOpen`）。

## 七、可观测性

- ✅ **就绪/健康端点**：`GET /readyz`（免鉴权）轻量探针，仅确认 HTTP 服务可用，不探测 SSH；
  `GET /healthz`（免鉴权）返回 `{status, hosts:[{name,online,available,message}]}`，
  SSH 主机经节流（2s）的 keepalive 探活。
- ✅ **Prometheus 指标**：`GET /metrics`（promhttp，免鉴权）暴露 `logviewer_ws_connections`、
  `logviewer_log_processes`、`logviewer_ssh_reconnects_total`、`logviewer_export_bytes_total`、
  `logviewer_log_bytes_sent_total`。
- ✅ **结构化日志**：`log_json`（默认 false 文本）、`log_level`（默认 info）、`log_file`
  （GUI 模式日志文件路径）配置；`internal/applog` 早期初始化 slog 并重定向标准库 `log`。
  GUI 模式下 stdout/stderr/Gin 全部重定向到日志文件；Web 模式走 stderr。高价值调用点
  （启动/启动查看/SSH 连接与重连/导出/重载/关闭）已迁移为带 `host`/`file`/`mode`/`shell`
  字段的结构化日志。

---

## 八、可靠性加固（已完成）

以下为按 P0/P1/P2 分级的一轮可靠性与体验加固，均已实现并通过测试：

**P0（阻断/数据错误）**

- ✅ **P0-1 行号栏滚动冻结**：改用 xterm 缓冲区绝对坐标（`baseY + length`）计算新增行，
  修复 scrollback 封顶后 `buf.length` 不再增长导致的行号永久冻结。
- ✅ **P0-2 SSH 进程 Start 失败会话泄漏**：`procmgr.Start` 在 `Start()` 返回错误时对称
  调用 `Kill()` 回收已分配的 SSH session/channel（远端命令可能已经开始执行）。
- ✅ **P0-3 单边时间范围过滤**：`TimeBounds` 支持只填开始/只填结束，命令层动态生成
  `t>=s`/`t<=e`，预览文案同步。
- ✅ **P0-4 WS 异步回调竞态**：引入连接代次 `wsGen` 守卫，旧连接的 onmessage/onclose
  不再污染新连接状态。
- ✅ **P0-5 暂停状态复位**：统一 `resetPauseState()`，切换文件/主机、断线、停止时清零
  暂停缓冲，避免旧日志串到新视图。
- ✅ **P0-6 离线点开始无反馈**：未连接时点开始明确提示并主动发起连接。
- ✅ **Ctrl+F 终端搜索无效**：开启 `allowProposedApi`，使 search addon 的匹配高亮
  装饰 API 不再抛错（被 try/catch 吞掉导致看似"搜不到"）。
- ✅ **local-web-02 平台探测失败**：根因是 Win32-OpenSSH 默认 shell 为 cmd.exe，
  嵌套 `cmd /c ver` 触发引号 bug；改为裸 `ver` + PowerShell 兜底探测。

**P1（健壮性）**

- ✅ **P1-1 保存预设丢失注释**：`configs` 键被注释掉时，用 hujson AST 原位插入子树，
  不再全量 Marshal 剥光注释。
- ✅ **P1-2 重复 ResolvePath**：明确接口契约——`Ls` 内部解析、`Stat/Open` 接收已规范化
  路径，消除 SSH 下多余的 SFTP `RealPath` 往返。
- ✅ **P1-3 后端参数范围校验**：`LogConfig.Validate` 限制读取行数（≤100 万、禁负）、
  上下文行数（≤1 万、禁负）；`appconfig.Validate` 校验端口/超时范围。
- ✅ **P1-4 断线会话自动恢复**：跟踪中断线保留 `pendingResume`，重连后自动续跟。
- ✅ **P1-5/6/7 交互细节**：配置按钮统一 `safeRun` 捕获异常；Windows 正则错误带
  "匹配/排除"前缀以标红对应输入框；目录展开加在途锁防并发请求；编码下拉补 GB2312；
  非安全上下文剪贴板兜底；主机名属性选择器改为按值查找防注入；删除无用的 gutterFit；
  补"重命名"按钮接线。

**P2（体验/并发打磨，P2-1 加密 KDF 升级经确认不做）**

- ✅ **P2-2 热加载优雅排空**：`Rebuild` 返回被替换/移除的主机别名，服务端通过 WS
  `reconnect` 指令通知对应连接迁移到新实例，旧实例随后 Close。
- ✅ **P2-3 SSH 在途操作重连**：新增 `invalidateConn(old)` 比较拆除，修复并发在途操作
  中一个失败会误杀另一个刚建好的新连接的竞态；SFTP/session/keepalive 统一使用。
- ✅ **P2-4 WS 401 登录判定**：WS 握手失败（从未 onopen）时探测 `/api/auth/status`，
  会话过期则弹登录框而非无限重连。
- ✅ **P2-5 导出取消与刷新去重**：导出遮罩加"取消"按钮（AbortController，服务端随
  客户端断开 Kill 进程）；`refreshHosts` 加在途锁。
## 九、 想法清单
- **增加AI分析功能**，配置上模型地址和token，能进行分析建议。

