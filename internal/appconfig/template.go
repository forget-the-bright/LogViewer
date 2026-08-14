package appconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"logviewer/internal/config"
)

// defaultTemplate 返回带注释的初始配置文件内容。
// 注意：这是给用户第一次阅读用的带注释版本；程序之后通过界面保存时会用标准 JSON
// 重写，注释会丢失（文件顶部已说明）。
func defaultTemplate() string {
	return `{
  // ====================================================================
  // LogViewer 配置文件（支持 // 与 /* */ 注释）
  //
  // 注意：通过 Web 界面保存"过滤预设"时，程序会用标准 JSON 重写本文件，
  // 手动添加的注释会被移除。需要保留的说明请写到外部文档。
  // 文件含 SSH/登录密码，权限会设为 0600，请勿提交到代码仓库。
  // ====================================================================

  // HTTP 监听地址。默认只监听本机回环。
  "addr": ":8080",

  // 登录认证。enabled=false（默认）时完全不做登录校验，所有功能直接可用。
  // 若需要登录页，把 enabled 改为 true 并填写 username/password。
  // password 推荐用 bcrypt 哈希：在命令行执行
  //   logviewer -hash-password 你的密码
  // 把输出贴进来。明文也能用但会降低安全性。
  "auth": {
    "enabled": false,
    "username": "",
    "password": "",
    "session_ttl_minutes": 720
  },

  // 机器列表。键名即界面上显示的别名；"local" 是内置本机，无需 ssh 字段。
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
  //   "platform": "",                  // 留空自动探测（linux/darwin/windows）
  //   "dirs": ["/var/log/nginx"],
  //   "configs": { "default_name": "默认配置", "configs": {} }
  // }
  "hosts": {
    "local": {
      // 本机扫描目录；命令行 -dir 传入的目录会追加到这里并去重。
      "dirs": [],
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
  }
}
`
}

// GenerateTemplate 在 path 写入带注释的默认配置。若 migrated 非 nil，
// 把迁移来的 configs 填入 local 主机。
func GenerateTemplate(path string, migrated *config.ConfigStore) error {
	content := defaultTemplate()
	if migrated != nil {
		// 用迁移数据替换模板里的 local.configs
		cfg := &AppConfig{}
		// 解析模板（去注释）
		val, err := parseJSONC([]byte(content))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(val, cfg); err != nil {
			return err
		}
		cfg.Hosts["local"] = HostConfig{Configs: *migrated}
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		content = string(b) + "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
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
