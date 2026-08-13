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

- **目录浏览**：懒加载目录树，根目录可配置，内置路径穿越防护。
- **实时跟踪 / 静态查看**：跟踪等价于 `tail -F`（Windows `Get-Content -Wait`），天然兼容
  logrotate；静态模式一次性读取末 N 行或全文。
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
- **无残留进程**：停止跟踪或断开 WebSocket 时杀掉整条进程组。

---

## 快速开始

### 从源码运行

要求 Go 1.26+。

```bash
go run .
# 浏览器打开 http://127.0.0.1:8080
```

### 命令行参数

| 参数     | 默认    | 说明                                                         |
| -------- | ------- | ------------------------------------------------------------ |
| `-addr`  | `:8080` | HTTP 监听地址                                                |
| `-dir`   | （空）  | 允许扫描的根工作目录，逗号 / 分号分隔，可多个。为空时默认用进程当前工作目录 |

示例：

```bash
# Windows：把两个日志目录都暴露出来
logviewer.exe -addr 127.0.0.1:9000 -dir "D:\logs,C:\tomcat\logs"

# Linux/macOS
./logviewer -addr :9000 -dir /var/log,/opt/app/logs
```

### 配置文件

首次运行会在可执行文件同目录的 `config/configs.json` 写入内置默认配置。可以直接备份、
拷贝这个文件迁移配置。

---

## 从源码构建

```bash
# 当前平台
go build -o dist/logviewer .

# 交叉编译（示例：Linux amd64）
GOOS=linux GOARCH=amd64 go build -o dist/logviewer .
```

Windows 下一键全平台打包（6 个目标：windows/linux/darwin × amd64/arm64）：

```powershell
# 注意：脚本里的 $MainPath 需与实际入口一致（本项目入口为根目录 main.go）
.\build_all_platforms.ps1
```

产物输出到 `dist/`。前端资源已嵌入二进制，部署时只需要拷贝单个可执行文件。

---

## 目录结构

```
fsdownload/
├── main.go                     # 入口：embed 静态资源、装载模块、启动 Gin
├── go.mod / go.sum
├── build_all_platforms.ps1     # 跨平台打包脚本
├── static/                     # 前端（embed 进二进制）
│   ├── index.html
│   ├── app.js
│   ├── style.css
│   └── vendor/                 # xterm.js / flatpickr 本地 vendor，无需联网
├── config/                     # 运行时用户配置目录（configs.json）
├── internal/
│   ├── config/                 # 配置结构 + 持久化 CRUD
│   ├── cmdbuild/               # 跨平台命令构建 + 过滤参数拼装
│   ├── procmgr/                # 子进程管理：启动/读取/节流/杀进程组
│   └── server/                 # Gin 路由 + 目录浏览 + 配置 API + 导出 + WebSocket
└── docs/                       # 设计与经验文档
```

---

## 文档

- [架构设计](docs/architecture.md)
- [原生命令管道设计](docs/native-command-pipeline.md) —— "Go 只是外壳"的具体实现
- [开发指南](docs/development.md)
- [部署说明](docs/deployment.md)
- [经验总结 / 踩坑记录](docs/lessons.md)

---

## 安全说明

- 仅监听本机回环地址时适合个人本地使用；如需远程访问请自行加认证反代。
- 所有文件访问都限制在配置的根目录内，后端做了 `filepath.Abs` + `filepath.Rel` 路径穿越校验。
- 命令构建使用参数化引用（Unix 单引号转义、PowerShell 单引号双写转义），避免 shell 注入。
