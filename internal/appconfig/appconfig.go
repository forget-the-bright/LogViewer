// Package appconfig 负责 logviewer.json 的加载、生成、迁移与持久化。
//
// 配置文件支持 JSONC 注释（// 与 /* */），便于人工阅读；但程序通过界面保存
// 过滤预设时会用标准 encoding/json 重新序列化，注释会被移除——首次生成的模板
// 顶部已对此作出说明。
package appconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/hujson"
	"golang.org/x/crypto/bcrypt"

	"logviewer/internal/config"
)

// 默认配置文件名。
const FileName = "logviewer.json"

// AppConfig 是 logviewer.json 的顶层结构。
type AppConfig struct {
	Addr  string                `json:"addr"`
	Auth  AuthConfig            `json:"auth"`
	Hosts map[string]HostConfig `json:"hosts"`
}

// AuthConfig 控制登录认证。Enabled 为 false（默认）时完全不做登录校验，
// 所有功能直接可用；设为 true 且 Username 非空才启用登录页与会话校验。
type AuthConfig struct {
	Enabled           bool   `json:"enabled"`
	Username          string `json:"username"`
	Password          string `json:"password"` // 明文或 bcrypt 哈希（$2a$/$2b$ 开头）
	SessionTTLMinutes int    `json:"session_ttl_minutes"`
}

// IsBcryptHash 判断密码字段是否已是 bcrypt 哈希。
func IsBcryptHash(s string) bool {
	return strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$") || strings.HasPrefix(s, "$2y$")
}

// ValidatePassword 用配置里的密码校验明文。哈希走 bcrypt；明文走常量时间比较。
func (a AuthConfig) ValidatePassword(plain string) bool {
	if !a.Enabled || a.Username == "" {
		return false // 未启用认证
	}
	if IsBcryptHash(a.Password) {
		return bcrypt.CompareHashAndPassword([]byte(a.Password), []byte(plain)) == nil
	}
	return subtleEqual(a.Password, plain)
}

// HashPassword 生成 bcrypt 哈希，供 -hash-password 子命令使用。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// SSHConfig 描述一台远程机器的 SSH 连接参数。阶段一仅解析与持久化，不实际连接。
type SSHConfig struct {
	Host                  string `json:"host"`
	Port                  int    `json:"port"`
	Username              string `json:"username"`
	Password              string `json:"password"`
	KnownHostsFile        string `json:"known_hosts_file"`
	InsecureSkipHostKey   bool   `json:"insecure_skip_host_key"`
	ConnectTimeoutSeconds int    `json:"connect_timeout_seconds"`
	KeepAliveSeconds      int    `json:"keepalive_seconds"`
}

// HostConfig 是一台机器（本机或远程）的配置。
type HostConfig struct {
	SSH      *SSHConfig          `json:"ssh,omitempty"`
	Platform string              `json:"platform,omitempty"`
	Dirs     []string           `json:"dirs"`
	Configs  config.ConfigStore `json:"configs"`
}

// Locate 按以下顺序查找配置文件：explicit → <exeDir>/logviewer.json → <cwd>/logviewer.json。
// 返回（路径, 是否存在）。
func Locate(explicit string) (string, bool, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		return abs, err == nil && fileExists(abs), err
	}
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), FileName))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, FileName))
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p, true, nil
		}
	}
	// 都不存在：默认落在 exe 目录（若不可写则退回 cwd）
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), FileName), false, nil
	}
	return filepath.Join(".", FileName), false, nil
}

// Load 读取并解析配置。path 为空时用 Locate 自动查找。
// extraDirs 是命令行 -dir 传入的目录，会合并到 hosts.local.dirs 并去重。
// 若文件不存在则生成模板（并尝试迁移旧 config/configs.json）。
func Load(path string, extraDirs []string) (*AppConfig, string, error) {
	if path == "" {
		p, _, err := Locate("")
		if err != nil {
			return nil, "", err
		}
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}

	if !fileExists(abs) {
		// 迁移旧配置
		migrated := tryMigrateOldConfig(abs)
		if err := GenerateTemplate(abs, migrated); err != nil {
			return nil, abs, err
		}
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, abs, err
	}
	standardized, err := parseJSONC(raw)
	if err != nil {
		return nil, abs, fmt.Errorf("解析 %s 失败: %w", abs, err)
	}
	var cfg AppConfig
	if err := json.Unmarshal(standardized, &cfg); err != nil {
		return nil, abs, fmt.Errorf("解析 %s 失败: %w", abs, err)
	}

	cfg.applyDefaults()
	cfg.mergeLocalDirs(extraDirs)
	if err := cfg.Validate(); err != nil {
		return nil, abs, err
	}
	return &cfg, abs, nil
}

// Save 把配置写回磁盘（JSON 缩进）。文件权限 0600（含密码）。
func Save(path string, cfg *AppConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (c *AppConfig) applyDefaults() {
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.Hosts == nil {
		c.Hosts = map[string]HostConfig{}
	}
	if _, ok := c.Hosts["local"]; !ok {
		c.Hosts["local"] = HostConfig{Configs: config.NewConfigStore()}
	}
	for name, h := range c.Hosts {
		if len(h.Configs.Configs) == 0 {
			h.Configs = config.NewConfigStore()
			c.Hosts[name] = h
		}
		if h.SSH != nil {
			if h.SSH.Port == 0 {
				h.SSH.Port = 22
			}
			if h.SSH.ConnectTimeoutSeconds == 0 {
				h.SSH.ConnectTimeoutSeconds = 10
			}
			if h.SSH.KeepAliveSeconds == 0 {
				h.SSH.KeepAliveSeconds = 30
			}
			c.Hosts[name] = h
		}
	}
	if c.Auth.SessionTTLMinutes == 0 {
		c.Auth.SessionTTLMinutes = 720
	}
}

// mergeLocalDirs 把命令行 -dir 合并进 hosts.local.dirs。
// 所有目录统一做 Abs+Clean 规范化后去重，保证配置里的路径与运行时解析一致。
func (c *AppConfig) mergeLocalDirs(extraDirs []string) {
	local := c.Hosts["local"]
	seen := map[string]bool{}
	out := make([]string, 0, len(local.Dirs)+len(extraDirs))
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			return
		}
		abs = filepath.Clean(abs)
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	for _, d := range local.Dirs {
		add(d)
	}
	for _, d := range extraDirs {
		add(d)
	}
	local.Dirs = out
	c.Hosts["local"] = local
}

// Validate 做结构性校验（阶段一不实际连接 SSH，只校验字段完整性）。
func (c *AppConfig) Validate() error {
	if len(c.Hosts) == 0 {
		return errors.New("hosts 不能为空")
	}
	for name, h := range c.Hosts {
		if name == "" {
			return errors.New("机器别名不能为空")
		}
		if name == "local" && h.SSH != nil {
			return errors.New("local 是内置本机别名，不能配置 ssh 字段")
		}
		if h.SSH != nil {
			if h.SSH.Host == "" {
				return fmt.Errorf("机器 %q 的 ssh.host 不能为空", name)
			}
			if h.SSH.Username == "" {
				return fmt.Errorf("机器 %q 的 ssh.username 不能为空", name)
			}
			if h.SSH.Password == "" {
				return fmt.Errorf("机器 %q 的 ssh.password 不能为空（当前仅支持密码认证）", name)
			}
		}
		switch h.Platform {
		case "", "linux", "darwin", "windows":
		default:
			return fmt.Errorf("机器 %q 的 platform 非法: %q（可选 linux/darwin/windows）", name, h.Platform)
		}
	}
	return nil
}

// UpdateHostConfigs 用某个 host 的 Manager 最新快照回写配置（保存过滤预设时调用）。
func (c *AppConfig) UpdateHostConfigs(hostName string, store config.ConfigStore) {
	if h, ok := c.Hosts[hostName]; ok {
		h.Configs = store
		c.Hosts[hostName] = h
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// parseJSONC 用 hujson 容忍注释/尾逗号，标准化为普通 JSON 字节。
func parseJSONC(raw []byte) ([]byte, error) {
	huval, err := hujson.Parse(raw)
	if err != nil {
		return nil, err
	}
	huval.Standardize()
	return []byte(huval.String()), nil
}

// subtleEqual 常量时间字符串比较，避免明文密码比对的计时侧信道。
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
