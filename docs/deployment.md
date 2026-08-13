# 部署说明

LogViewer 编译后是**单个自包含可执行文件**：前端 HTML/JS/CSS 和 vendor 库都通过
`go:embed` 打进了二进制，部署时不需要额外的 Web 服务器、Node 运行时或静态文件目录。

## 1. 构建

### 当前平台

```bash
go build -o dist/logviewer .
```

### 交叉编译

```bash
# Linux amd64
GOOS=linux  GOARCH=amd64 go build -o dist/logviewer .
# Linux arm64
GOOS=linux  GOARCH=arm64 go build -o dist/logviewer .
# macOS
GOOS=darwin GOARCH=arm64 go build -o dist/logviewer .
# Windows
GOOS=windows GOARCH=amd64 go build -o dist/logviewer.exe .
```

### 一键全平台打包

在 Windows 上执行：

```powershell
.\build_all_platforms.ps1
```

会在 `dist/` 下生成 6 个压缩包（windows/linux/darwin × amd64/arm64）。

> 注意：脚本里的 `$MainPath` 必须指向 `main.go` 所在目录（本项目为根目录 `.`）。

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

| 参数    | 说明                                                                 |
| ------- | -------------------------------------------------------------------- |
| `-addr` | 监听地址，默认 `:8080`                                               |
| `-dir`  | 允许浏览的根目录，逗号或分号分隔；为空时用进程当前工作目录           |

### 运行时生成的文件

程序启动后，会在**可执行文件所在目录**下创建：

```
config/configs.json     # 用户保存的过滤配置
```

这个文件可以备份、随安装包一起分发；删除它会在下次启动时重建默认配置。

## 3. 平台依赖

| 平台          | 依赖                                                                      |
| ------------- | ------------------------------------------------------------------------- |
| Linux         | `tail`、`cat`、`grep`、`awk`、`iconv`（GBK 时需要，多数发行版自带）        |
| macOS         | 同上（系统自带 BSD 版本）                                                 |
| Windows       | PowerShell（Windows 7+ / Server 2008 R2+ 自带）                           |

Windows 上整条命令管道在单个 powershell 进程内执行，因此停止时杀掉 powershell
就会终止整条管道，不会留下孤立的 `Get-Content`。

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

## 5. 反向代理与远程访问

程序没有内置认证，默认建议只监听 `127.0.0.1`。需要多人访问时，放在带认证的反向代理后。

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

## 6. 升级

1. 停止旧进程（systemd: `systemctl stop logviewer`）；
2. 用新二进制覆盖；
3. 启动。`config/configs.json` 向前兼容，新增字段会在保存时补齐，旧配置不受影响。

## 7. 安全建议

- 不要把 `-addr` 直接暴露在公网；走反向代理并加鉴权。
- `-dir` 只授予需要查看的日志目录最小权限，不要给整个磁盘根目录。
- 运行账号对日志目录保持只读即可，程序本身不写日志文件。
