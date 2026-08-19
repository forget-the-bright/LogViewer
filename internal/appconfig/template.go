package appconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"logviewer/internal/config"
)

// defaultTemplate 返回带注释的初始配置文件内容。
// Web 界面保存过滤预设时通过 hujson AST 局部补丁写回，注释与格式均保留。
func defaultTemplate() string {
	return `{
  // ====================================================================
  // LogViewer 配置文件（支持 // 与 /* */ 注释）
  //
  // 通过 Web 界面保存"过滤预设"时，程序仅替换对应主机的 configs 子树，
  // 其余位置的注释与格式保持不变。
  // 文件含 SSH/登录密码，权限会设为 0600，请勿提交到代码仓库。
  //
  // 密码加密：可用 ` + "`logviewer -encrypt-config -key <密钥>`" + ` 加密所有密码字段，
  // 启动时通过 -key 或 LOGVIEWER_KEY 环境变量提供密钥解密。
  // ====================================================================

  // HTTP 监听地址。默认只监听本机回环。
  "addr": ":8080",

  // 登录认证。enabled=false（默认）时完全不做登录校验，所有功能直接可用。
  // 若需要登录页，把 enabled 改为 true 并填写 username/password。
  // password 推荐用 bcrypt 哈希：在命令行执行
  //   logviewer -hash-password 你的密码
  // 把输出贴进来。明文也能用但会降低安全性；也可整体加密。
  "auth": {
    "enabled": false,
    "username": "",
    "password": "",
    "session_ttl_minutes": 720
  },

  // 服务端自身日志输出。
  // log_json=true 输出 JSON 结构化日志（便于日志采集系统消费）；false 输出人类可读文本。
  "log_json": false,
  // 日志级别：debug / info / warn / error，默认 info。
  "log_level": "info",
  // 日志文件路径（仅 GUI 模式生效）。每次启动覆盖（截断）旧文件；留空则默认可执行文件
  // 同目录下的 logviewer-gui.log。Web 模式日志输出到 stderr，由运行命令重定向。
  // 相对路径相对于可执行文件所在目录解析。
  // "log_file": "",
  // 开发调试用：true 时把每条发往目标机器的查询/导出/正则校验命令打印到服务端日志。
  // 默认 false。生产环境不建议开启（follow 模式会持续输出，且命令可能包含文件路径）。
  "log_commands": false,
  // Gin 调试模式：true 时启用 gin 路由调试，便于开发时调试。默认 false。
  "gin_mode_debug": false,
  // follow 跟踪模式下，WebSocket 断线后服务端保留会话（继续缓冲日志）的宽限秒数。
  // 在此期间重连可自动补齐断连间隙的日志；超时则回收进程。默认 45，范围 5-3600。
  "session_grace_seconds": 45,


  "hosts": {
    "local": {
      // 前端显示的友好名称（留空则显示 "local"）。本机会自动追加 "-local" 后缀。
      // "display_name": "我的机器",
      // 本机扫描目录；命令行 -dir 传入的目录会追加到这里并去重。
      "dirs": [],
      // 目录树展示的文件后缀（不区分大小写，可省略前导点）。
      // 缺省为 [".log", ".out"]；设为 ["*"] 展示目录下所有文件。目录始终展示。
      "file_extensions": [".log", ".out"],
      // 每台机器独立的过滤预设。
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
              "TimeStart": "",
              "TimeEnd": "",
              "TimePrecision": "second",
              "Levels": ["ERROR", "WARN"],
              "Content": "",
              "Exclude": "",
              "CustomRegex": ""
            },
            "HighlightRules": ["ERROR", "WARN"]
          }
        }
      }
    }
    // 机器列表。键名是内部标识（API 路由用），display_name 是界面显示名（留空则显示键名）。
    // "local" 是内置本机，无需 ssh 字段；非 "local" 的主机必须配置 ssh。
    // 添加远程机器示例（取消注释并修改）：
    //
    // "prod-web-01": {
    //   "ssh": {
    //     "host": "10.0.0.11",
    //     "port": 22,
    //     "username": "root",
    //     "password": "changeme",
    //     "known_hosts_file": "",        // 留空用 ~/.ssh/known_hosts，首次自动 TOFU
    //     "insecure_skip_host_key": false,
    //     "connect_timeout_seconds": 10,
    //     "keepalive_seconds": 30
    //   },
    //   "display_name": "生产-Web-01",   // 前端显示的友好名称（留空则显示键名）
    //   "platform": "",                  // 留空自动探测（linux/darwin/windows）
    //   "dirs": ["/var/log/nginx"],
    //   "file_extensions": [".log", ".out"],  // 目录树展示的文件后缀，缺省 .log/.out；["*"] 展示全部
    //   "configs": { "default_name": "默认配置", "configs": {} }
    // }
  }
}
`
}

// GenerateTemplate 在 path 写入带注释的默认配置。若 migrated 非 nil，
// 把迁移来的 configs 原位填入 local 主机的 configs 子树，保留模板的注释、
// 格式以及注释掉的远程主机示例。
func GenerateTemplate(path string, migrated *config.ConfigStore) error {
	content := []byte(defaultTemplate())
	if migrated != nil {
		// 仅替换 /hosts/local/configs 子树，不整体 Marshal（那会剥光注释）。
		out, n, err := spliceRawValues(content, []splice{
			{ptr: "/hosts/local/configs", newValue: *migrated},
		})
		if err != nil {
			return err
		}
		if n == 0 {
			// 模板结构异常（理论上不会发生），回退到带迁移数据的标准 JSON。
			b, err := json.MarshalIndent(AppConfig{
				Addr: ":8080",
				Auth: AuthConfig{SessionTTLMinutes: 720},
				Hosts: map[string]HostConfig{
					"local": {Configs: *migrated},
				},
			}, "", "  ")
			if err != nil {
				return err
			}
			content = append(b, '\n')
		} else {
			content = out
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o600)
}

// tryMigrateOldConfig 查找旧版 <exe>/config/configs.json 并迁入。
// 成功后把旧文件改名为 configs.json.bak，返回迁移后的 store。
func tryMigrateOldConfig(newConfigPath string) *config.ConfigStore {
	oldDir := filepath.Join(filepath.Dir(newConfigPath), "config")
	oldPath := filepath.Join(oldDir, "configs.json")
	b, err := os.ReadFile(oldPath)
	if err != nil {
		return nil
	}
	var store config.ConfigStore
	if err := json.Unmarshal(b, &store); err != nil {
		fmt.Fprintf(os.Stderr, "警告：旧配置 %s 解析失败，跳过迁移: %v\n", oldPath, err)
		return nil
	}
	if len(store.Configs) == 0 {
		return nil
	}
	// 备份旧文件
	_ = os.Rename(oldPath, oldPath+".bak")
	fmt.Fprintf(os.Stderr, "已迁移旧配置 %s → %s（旧文件备份为 .bak）\n", oldPath, newConfigPath)
	return &store
}
