# 部署说明

LogViewer 编译后是**单个自包含可执行文件**：前端 HTML/JS/CSS 和 vendor 库都通过
`go:embed` 打进了二进制，部署时不需要额外的 Web 服务器、Node 运行时或静态文件目录。

## 1. 构建

### 版本号

版本号唯一来源是项目根目录的 `VERSION` 文件，构建时通过
`-ldflags "-X main.version=<version>"` 注入到二进制，启动日志会打印
`LogViewer <version> 启动...`。**版本号只由开发者手动修改 `VERSION`，AI/自动化不得改动。**

### 当前平台

```bash
# 直接构建（版本号显示为 dev）
go build -o dist/logviewer .

# 带版本号构建（VERSION 去掉前后空白/换行）
go build -ldflags "-s -w -X main.version=$(cat VERSION)" -o dist/logviewer .
```

### 交叉编译

```bash
VER=$(cat VERSION | tr -d '\r\n')
# Linux amd64
GOOS=linux  GOARCH=amd64 go build -ldflags "-s -w -X main.version=$VER" -o dist/logviewer .
# Linux arm64
GOOS=linux  GOARCH=arm64 go build -ldflags "-s -w -X main.version=$VER" -o dist/logviewer .
# macOS
GOOS=darwin GOARCH=arm64 go build -ldflags "-s -w -X main.version=$VER" -o dist/logviewer .
# Windows
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -X main.version=$VER" -o dist/logviewer.exe .
```

### 一键全平台打包

在 Windows 上执行：

```powershell
.\build_all_platforms.ps1
```

脚本自动从 `VERSION` 读取版本号，在 `dist/` 下生成 6 个压缩包
（windows/linux/darwin × amd64/arm64），包名形如
`logviewer-<version>-linux-amd64.tar.gz`。这些是 **web-only** 二进制（不含 Wails、
无 CGO），运行后是浏览器访问的 HTTP 服务。

### GUI 客户端（仅 Windows）

需要桌面客户端形态（双击开窗口，无需浏览器）时，在 Windows 上执行：

```powershell
.\build_gui.ps1
```

产出 `dist/logviewer-gui-<version>-windows-{amd64,arm64}.exe`。GUI 二进制内置 Gin 只监听
`127.0.0.1` 随机端口，运行日志写入 `%AppData%\LogViewer\logviewer-gui.log`。
目标机器需装有 WebView2 Runtime（Win11 自带）。Web-only 与 GUI 是两套构建产物，
Linux/macOS 目前只提供 web-only 形态。

## 2. 运行

把可执行文件拷到目标机器，直接运行：

```bash
# Linux/macOS
./logviewer -addr 127.0.0.1:8080 -dir /var/log,/opt/app/logs

# Windows
logviewer.exe -addr 127.0.0.1:8080 -dir "D:\logs,C:\tomcat\logs"
```

浏览器打开 `http://127.0.0.1:8080`。

### 参数

| 参数               | 说明                                                                 |
| ------------------ | -------------------------------------------------------------------- |
| `-addr`            | 监听地址，默认 `:8080`（覆盖 logviewer.json 中的 addr）              |
| `-dir`             | 允许浏览的根目录，逗号或分号分隔；合并到本机 local 的 dirs 并去重    |
| `-config`          | 显式指定配置文件路径                                                 |
| `-hash-password`   | 生成 bcrypt 密码哈希后退出（用于配置 auth.password）                 |
| `-key`             | 配置密码解密密钥（也可通过 `LOGVIEWER_KEY` 环境变量传入）            |
| `-encrypt-config`  | 加密配置文件中所有明文密码后退出（需配合 `-key`）                    |
| `-decrypt-config`  | 解密配置文件中所有加密密码后退出（需配合 `-key`）                    |

### 运行时生成的文件

程序启动后，会在**可执行文件所在目录**下查找/生成：

```
logviewer.json     # 全部配置：监听地址、认证、机器列表、扫描目录、过滤预设
```

查找顺序：`-config` 指定路径 → `<exe>/logviewer.json` → `<cwd>/logviewer.json`；
都不存在时在 `<exe>` 目录生成带注释的模板。文件权限 `0600`（含密码）。

旧版的 `config/configs.json` 会在首次启动时自动迁移到 `logviewer.json`，
旧文件备份为 `configs.json.bak`。

## 3. 平台依赖

| 平台          | 依赖                                                                      |
| ------------- | ------------------------------------------------------------------------- |
| Linux         | `tail`、`cat`、`grep`、`awk`、`iconv`（GBK 时需要，多数发行版自带）        |
| macOS         | 同上（系统自带 BSD 版本）                                                 |
| Windows       | PowerShell（Windows 7+ / Server 2008 R2+ 自带）                           |

Windows 上整条命令管道在单个 powershell 进程内执行，停止时会用 `taskkill /T /F`
终止整个进程树，不会留下孤立的 `Get-Content`。受 Windows OpenSSH 通道关闭机制影响，
远程 Windows 停止可能有 ~1s 的回收延迟（不影响后续操作，界面会显示"停止中"loading）。

## 4. 作为后台服务运行

### Linux（systemd）

新建 `/etc/systemd/system/logviewer.service`：

```ini
[Unit]
Description=LogViewer
After=network.target

[Service]
ExecStart=/opt/logviewer/logviewer -addr 127.0.0.1:8080 -dir /var/log
WorkingDirectory=/opt/logviewer
Restart=on-failure
User=your-user

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now logviewer
```

### Windows

用任务计划程序"登录时启动"或 [NSSM](https://nssm.cc/) 把 `logviewer.exe` 注册成服务。
注意 `-dir` 要指定能读到日志的绝对路径。

## 5. 登录认证

默认 `auth.enabled=false`，**完全不做登录校验**，适合只监听本机回环的个人使用。
若要绑定到非本机地址或放在网络中，建议启用内置登录：

1. 生成密码哈希：
   ```bash
   ./logviewer -hash-password 'your-password'
   # 输出形如 $2a$10$xxxxxxxx...
   ```
2. 在 `logviewer.json` 中填写（明文密码也能用但启动会有警告，不推荐）：
   ```json
   "auth": {
     "enabled": true,
     "username": "admin",
     "password": "$2a$10$xxxxxxxx...",
     "session_ttl_minutes": 720
   }
   ```
3. 重启后浏览器会出现登录遮罩，登录后凭 `HttpOnly; SameSite=Lax` 会话 Cookie
   访问（HTTPS 下自动加 `Secure`）。

实现要点：

- 会话 token 为 32 字节随机值（crypto/rand + base64url），仅存内存，重启失效；
  每次请求滑动续期，超过 `session_ttl_minutes` 无活动自动过期。
- 登录失败统一等待约 1 秒并按 IP 限流：60 秒内连续失败 5 次会短暂锁定，
  即使密码正确也拒绝，避免用户名枚举与爆破。
- WebSocket 在启用认证时校验会话 Cookie，并做 Origin 同源校验（仅比较 host，
  允许本地 http/ws 混用），防止跨站 WS 劫持。
- 登出立即失效服务端会话。

### 密码加密

配置文件中的 SSH 密码和登录密码可用 AES-256-GCM 加密存储，避免明文落盘。
密钥由用户提供，通过 SHA-256 派生为 32 字节密钥；每次加密使用随机 nonce，
加密结果以 `enc:v1:` 前缀标识。

```bash
# 1. 先编辑好 logviewer.json（密码用明文填写），然后加密：
./logviewer -encrypt-config -key 'your-passphrase'

# 2. 启动时提供密钥（二选一）：
./logviewer -key 'your-passphrase'
LOGVIEWER_KEY='your-passphrase' ./logviewer

# 3. 需要改回明文时：
./logviewer -decrypt-config -key 'your-passphrase'
```

规则：
- SSH 密码：明文会被加密；已是 `enc:v1:` 则跳过。
- 登录密码：明文会被加密；bcrypt 哈希（`$2a$` 等前缀）保持不变。
- 配置含加密密码但启动时未提供 `-key`/`LOGVIEWER_KEY`，程序会直接报错退出。
- 解密只在内存中进行，不会写回磁盘；只有显式传 `-encrypt-config`/`-decrypt-config`
  才修改文件，写回时保持权限 `0600`。
- 密钥不要落盘、不要写进脚本或 systemd unit；推荐用环境变量或 secrets 管理工具传入。

## 6. 热加载配置

运行时可以不重启重新加载 `logviewer.json`：

- **前端**：顶栏点击"重载配置"按钮（`POST /api/reload`，需登录）。
- **Unix**：`kill -HUP <pid>` 发送 SIGHUP 信号。

热加载会重新读取配置、重建主机集合：连接参数未变的 SSH 主机会话保留（不中断正在
跟踪的日志），新增/删除/配置变更的主机自动加入或关闭。认证开关或用户名变化时会
清空所有现有会话，要求重新登录。Windows 不支持 SIGHUP，只能通过前端按钮触发。

## 7. 反向代理与远程访问

默认建议只监听 `127.0.0.1`。启用上一节的内置登录后可直接绑定到内网地址；
需要多人访问、TLS 终止或更复杂鉴权时，也可放在反向代理之后。

Nginx 示例（注意要支持 WebSocket）：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;       # WebSocket 必需
    proxy_set_header Connection "upgrade";        # WebSocket 必需
    proxy_set_header Host $host;
    proxy_read_timeout 3600s;                     # 长连接不要过早超时
}
```

## 8. 升级

1. 停止旧进程（systemd: `systemctl stop logviewer`）；
2. 用新二进制覆盖；
3. 启动。`logviewer.json` 向前兼容，新增字段会在保存时补齐，旧配置不受影响。

## 9. 安全建议

- 不要把 `-addr` 直接暴露在公网；启用内置登录或走反向代理并加鉴权。
- `-dir` 只授予需要查看的日志目录最小权限，不要给整个磁盘根目录。
- 运行账号对日志目录保持只读即可，程序本身不写日志文件。
