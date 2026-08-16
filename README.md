# LogViewer

一个跨平台的本地日志查看工具。后端用 Go（Gin + WebSocket），前端用原生 HTML/JS +
[xterm.js](https://xtermjs.org/)，**单二进制自包含**（前端资源通过 `go:embed` 打包进可执行文件）。

核心理念：**一切操作都是命令，Go 只是外壳。** 日志的读取、跟踪、过滤、转码全部交给操作系统
原生命令完成（Linux/macOS：`tail` / `cat` / `awk` / `grep`；Windows：PowerShell
`Get-Content` / `Where-Object` / `Select-String`），Go 只负责把界面配置翻译成命令、启动进程、
把字节流转发给浏览器。详见 [docs/architecture.md](docs/architecture.md)。

---
<img width="1920" height="911" alt="image" src="https://github.com/user-attachments/assets/7c1faee2-53fe-474f-92a4-ecc317a9da3f" />

## 功能特性

- **目录浏览**：懒加载目录树，根目录可配置，内置路径穿越防护（含符号链接逃逸检测）。
- **多机器 / SSH 远程**：顶栏切换本机与远程 SSH 机器，自动探测远端平台（Linux/macOS/Windows），
  远程目录浏览与原始文件下载走 SFTP，远程日志查看/跟踪/过滤/导出走 SSH 原生命令管道，
  主机密钥 TOFU（首次写入 known_hosts，之后严格校验），断线自动重连、keepalive 保活。
- **实时跟踪 / 静态查看**：跟踪等价于 `tail -F`（Windows `Get-Content -Wait`），天然兼容
  logrotate；静态模式一次性读取末 N 行或全文。本机与远程共用同一套命令构建与流读取逻辑。
- **远端能力自适应**：连接时批量探测远端是否具备 `tail/cat/grep/awk/iconv`（最小化容器可能
  缺 `iconv`/`awk`）；缺失时前端自动禁用 GBK 转码（需 iconv）与时间范围过滤（需 awk），
  而不是命令静默失败。
- **过滤规则构建器**：
  - 时间范围（按天/时/分/秒四种粒度，字符串比较，命令长度恒定，不靠正则枚举时刻）
  - 日志级别（OR）
  - 内容关键词（字面量 / 正则两种模式）
  - 排除关键词（独立一轮反转过滤）
  - 上下文行（匹配前后 N 行，`grep -B/-A` / `Select-String -Context`）
  - 大小写敏感、反转匹配
- **编码支持**：UTF-8、GBK / GB2312（Unix 用 `iconv`，Windows 用 .NET 编码）。
- **行号显示**：基于 xterm.js 双终端 gutter 方案，逻辑行号与自动换行续行严格对齐。
- **高亮**：前端按关键词着色，可配置多条规则。
- **配置管理**：多套过滤配置保存/另存/重命名/删除/设为默认，持久化到 JSON。
- **导出**：原始文件下载；按当前表单过滤条件导出（流式，浏览器端带进度条）。
- **主题**：明 / 暗两套主题，xterm 同步切换。
- **登录认证（可选）**：`auth.enabled=true` 开启会话 Cookie 登录（bcrypt 密码、滑动续期、
  失败限流、WebSocket 同源校验、登出）；默认关闭，关闭时所有功能直接可用。详见
  [部署说明](docs/deployment.md)。
- **密码加密**：AES-256-GCM 加密配置中的 SSH/登录密码（`enc:v1:` 前缀），密钥通过
  `-key` 或 `LOGVIEWER_KEY` 提供；兼容明文模式，支持 `-encrypt-config` / `-decrypt-config`
  一次性加解密。
- **配置热加载**：前端"重载配置"按钮或 Unix `SIGHUP` 信号运行时重新加载配置，未变更的
  SSH 主机会话保留，正在跟踪的日志不中断。
- **注释保留**：配置文件支持 JSONC 注释，界面保存过滤预设时通过 hujson AST 局部补丁写回，
  不破坏其余位置的注释与格式。
- **断线反馈**：WebSocket 断线显示红色横幅，指数退避重连（1s→30s），"立即重连"按钮可手动触发；
  机器列表每 10 秒静默刷新在线状态。
- **可拖拽侧栏**：目录树与内容区之间的分隔条可拖拽改宽（180–600px），宽度记忆到浏览器，
  长文件名自动省略号并悬停显示全名。
- **无残留进程**：停止跟踪或断开 WebSocket 时杀掉整条进程组；退出时优雅关闭，先停进程再断 SSH。

---

## 快速开始

### 从源码运行

要求 Go 1.26+。

```bash
go run .
# 浏览器打开 http://127.0.0.1:8080
```

### 命令行参数

| 参数               | 默认    | 说明                                                         |
| ------------------ | ------- | ------------------------------------------------------------ |
| `-addr`            | `:8080` | HTTP 监听地址（覆盖 logviewer.json 中的 addr）               |
| `-dir`             | （空）  | 允许扫描的根工作目录，逗号 / 分号分隔，可多个。会合并到本机 local 主机的 dirs 并去重 |
| `-config`          | （空）  | 显式指定配置文件路径                                         |
| `-hash-password`   | （空）  | 传入明文密码，生成 bcrypt 哈希后退出（用于配置 auth.password） |
| `-key`             | （空）  | 配置密码解密密钥（也可通过 `LOGVIEWER_KEY` 环境变量传入）    |
| `-encrypt-config`  | `false` | 加密配置文件中所有明文密码后退出（需配合 `-key`）            |
| `-decrypt-config`  | `false` | 解密配置文件中所有加密密码后退出（需配合 `-key`）            |

示例：

```bash
# Windows：把两个日志目录都暴露出来
logviewer.exe -addr 127.0.0.1:9000 -dir "D:\logs,C:\tomcat\logs"

# Linux/macOS
./logviewer -addr :9000 -dir /var/log,/opt/app/logs

# 生成登录密码哈希
./logviewer -hash-password 'your-password'

# 加密配置中的明文密码
./logviewer -encrypt-config -key 'your-passphrase'

# 用密钥启动（配置中密码已加密）
./logviewer -key 'your-passphrase'
```

### 配置文件

首次运行会在**可执行文件同目录**生成 `logviewer.json`（查找顺序：`-config` 指定路径
→ `<exe>/logviewer.json` → `<cwd>/logviewer.json`）。文件支持 `//` 与 `/* */` 注释，
内含监听地址、登录认证、机器列表、扫描目录、过滤预设等全部配置。

- 旧版 `config/configs.json` 会在首次启动时自动迁移到 `logviewer.json` 的 `hosts.local.configs`，
  旧文件备份为 `configs.json.bak`。
- 文件含密码，权限设为 `0600`，请勿提交到代码仓库。
- 通过界面保存过滤预设时，程序仅替换对应主机的 `configs` 子树（hujson AST 局部补丁），
  其余位置的注释与格式保持不变。

---

## 从源码构建

版本号唯一来源是根目录 `VERSION` 文件，通过 `-ldflags "-X main.version=..."` 注入。

```bash
# 当前平台
go build -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/logviewer .

# 交叉编译（示例：Linux amd64）
GOOS=linux GOARCH=amd64 go build -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/logviewer .
```

> `VERSION` 只能由开发者本人手动修改，AI/自动化不得改动（见 [CLAUDE.md](CLAUDE.md)）。

Windows 下一键全平台打包（自动读取 `VERSION`，6 个目标：windows/linux/darwin × amd64/arm64）：

```powershell
.\build_all_platforms.ps1
```

产物输出到 `dist/`，包名形如 `logviewer-v0.0.4-linux-amd64.tar.gz`。前端资源已嵌入二进制，
部署时只需要拷贝单个可执行文件。

---

## 目录结构

```
LogViewer/
├── main.go                     # 入口：embed 静态资源、装载模块、启动 Gin
├── VERSION                     # 版本号唯一来源（仅开发者手动修改）
├── go.mod / go.sum
├── build_all_platforms.ps1     # 跨平台打包脚本（从 VERSION 读版本号）
├── static/                     # 前端（embed 进二进制）
│   ├── index.html
│   ├── app.js
│   ├── style.css
│   └── vendor/                 # xterm.js / flatpickr 本地 vendor，无需联网
├── logviewer.json              # 运行时生成的配置文件（首次启动自动创建，勿提交）
├── internal/
│   ├── appconfig/              # logviewer.json 加载(JSONC)/生成/迁移/密码哈希/AST 局部补丁
│   ├── cryptoutil/             # AES-256-GCM 配置密码加解密
│   ├── host/                   # 机器抽象：Host 接口 + LocalHost + SSHHost（SSH/SFTP/远程命令）
│   ├── config/                 # LogConfig 结构 + 预设 CRUD（内存 + 持久化钩子）
│   ├── cmdbuild/               # 跨平台命令构建 + 过滤参数拼装
│   ├── procmgr/                # 子进程管理：启动/读取/节流/杀进程组
│   └── server/                 # Gin 路由 + 目录浏览 + 配置 API + 导出 + WebSocket + 热加载
└── docs/                       # 设计与经验文档（含 CHANGELOG）
```

---

## 文档

- [变更日志](docs/CHANGELOG.md)
- [架构设计](docs/architecture.md)
- [原生命令管道设计](docs/native-command-pipeline.md) —— "Go 只是外壳"的具体实现
- [开发指南](docs/development.md)
- [部署说明](docs/deployment.md)
- [经验总结 / 踩坑记录](docs/lessons.md)
- [产品体验建议清单](docs/product-suggestions.md)
- [SSH 远程与登录设计](SSH远程与登录设计.md) —— 设计记录（多机器/SSH 与登录认证均已实现）

---

## 安全说明

- 仅监听本机回环地址且不启用认证时适合个人本地使用；若绑定到非本机地址，程序会在启动时
  警告"未启用登录认证存在未授权访问风险"，建议把 `auth.enabled` 置为 true（密码用
  `-hash-password` 生成 bcrypt 哈希），或放在带认证的反向代理之后。
- 会话 Cookie 为 `HttpOnly; SameSite=Lax`，HTTPS（含反代 `X-Forwarded-Proto: https`）下自动加 `Secure`；
  启用认证时 WebSocket 做同源校验，防止跨站 WS 劫持。
- 所有文件访问都限制在配置的根目录内，后端做了 `filepath.Abs` + `filepath.Rel` 路径穿越校验
  （远程额外做 SFTP `RealPath` 符号链接逃逸检测）。
- 命令构建使用参数化引用（Unix 单引号转义、PowerShell `-EncodedCommand` UTF-16LE base64），避免 shell 注入。
