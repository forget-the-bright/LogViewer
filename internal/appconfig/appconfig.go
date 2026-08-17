// Package appconfig 负责 logviewer.json 的加载、生成、迁移与持久化。
//
// 配置文件支持 JSONC 注释（// 与 /* */），便于人工阅读。所有程序化写回
// （Web 保存过滤预设、加解密密码、迁移旧配置）都通过 hujson AST 字节拼接
// 原位替换目标子树，保留文件其余部分的注释、格式以及注释掉的远程主机示例。
// 只有异常回退（目标结构损坏无法定位）才会用标准 JSON 全量重写。
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
	"logviewer/internal/cryptoutil"
)

// 默认配置文件名。
const FileName = "logviewer.json"

// AppConfig 是 logviewer.json 的顶层结构。
type AppConfig struct {
	Addr  string                `json:"addr"`
	Auth  AuthConfig            `json:"auth"`
	Hosts map[string]HostConfig `json:"hosts"`

	// LogJSON 为 true 时以 JSON 格式输出自身日志（便于日志采集系统消费）。
	LogJSON bool `json:"log_json"`
	// LogLevel 控制日志级别：debug/info/warn/error，默认 info。
	LogLevel string `json:"log_level"`
	// LogCommands 为 true 时把每条发往目标机器的查询/导出/校验命令打印到服务端日志，
	// 用于开发调试（排查命令构造、性能问题）。默认 false；生产环境不建议开启
	// （follow 模式下会持续输出，且命令可能包含文件路径）。
	LogCommands bool `json:"log_commands"`
	// GinModeDebug 为 true 时启用 gin 调试模式，便于开发时调试路由。默认 false。
	GIN_MODE_DEBUG bool `json:"gin_mode_debug"`
	// SessionGraceSeconds 是 follow 模式下 WebSocket 断线后会话保留的宽限秒数，
	// 在此期间重连可补齐断连间隙的日志。默认 45 秒。
	SessionGraceSeconds int `json:"session_grace_seconds"`
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

// ValidatePassword 用配置里的密码校验明文。
// 密码可能是：bcrypt 哈希、AES 加密后解密的明文、或直接明文。
func (a AuthConfig) ValidatePassword(plain string) bool {
	if !a.Enabled || a.Username == "" {
		return false // 未启用认证
	}
	pw := a.Password
	if cryptoutil.IsEncrypted(pw) {
		// 加密密码：需要先由 DecryptPasswords 在启动时解密到内存；
		// 正常流程下不会走到这里（启动时已解密）。防御性处理：拒绝。
		return false
	}
	if IsBcryptHash(pw) {
		return bcrypt.CompareHashAndPassword([]byte(pw), []byte(plain)) == nil
	}
	return subtleEqual(pw, plain)
}

// HashPassword 生成 bcrypt 哈希，供 -hash-password 子命令使用。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// HasEncryptedPasswords 判断配置中是否存在 enc:v1: 加密的密码字段。
func (c *AppConfig) HasEncryptedPasswords() bool {
	if cryptoutil.IsEncrypted(c.Auth.Password) {
		return true
	}
	for _, h := range c.Hosts {
		if h.SSH != nil && cryptoutil.IsEncrypted(h.SSH.Password) {
			return true
		}
	}
	return false
}

// EncryptPasswords 加密所有明文密码字段（原地修改）。
// bcrypt 哈希（$2a$/$2b$ 开头）保持不变。
func (c *AppConfig) EncryptPasswords(passphrase string) error {
	if passphrase == "" {
		return errors.New("加密密钥不能为空")
	}
	// auth.password：明文才加密，bcrypt 哈希跳过
	if c.Auth.Password != "" && !IsBcryptHash(c.Auth.Password) && !cryptoutil.IsEncrypted(c.Auth.Password) {
		enc, err := cryptoutil.Encrypt(passphrase, c.Auth.Password)
		if err != nil {
			return fmt.Errorf("加密 auth.password 失败: %w", err)
		}
		c.Auth.Password = enc
	}
	// SSH 密码
	for name, h := range c.Hosts {
		if h.SSH == nil || h.SSH.Password == "" || cryptoutil.IsEncrypted(h.SSH.Password) {
			continue
		}
		enc, err := cryptoutil.Encrypt(passphrase, h.SSH.Password)
		if err != nil {
			return fmt.Errorf("加密机器 %q 的 ssh.password 失败: %w", name, err)
		}
		h.SSH.Password = enc
		c.Hosts[name] = h
	}
	return nil
}

// DecryptPasswords 解密所有 enc:v1: 密码字段（原地修改，仅内存中）。
func (c *AppConfig) DecryptPasswords(passphrase string) error {
	if passphrase == "" {
		return errors.New("解密密钥不能为空")
	}
	if cryptoutil.IsEncrypted(c.Auth.Password) {
		dec, err := cryptoutil.Decrypt(passphrase, c.Auth.Password)
		if err != nil {
			return fmt.Errorf("解密 auth.password 失败: %w", err)
		}
		c.Auth.Password = dec
	}
	for name, h := range c.Hosts {
		if h.SSH == nil || !cryptoutil.IsEncrypted(h.SSH.Password) {
			continue
		}
		dec, err := cryptoutil.Decrypt(passphrase, h.SSH.Password)
		if err != nil {
			return fmt.Errorf("解密机器 %q 的 ssh.password 失败: %w", name, err)
		}
		h.SSH.Password = dec
		c.Hosts[name] = h
	}
	return nil
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
	SSH            *SSHConfig         `json:"ssh,omitempty"`
	Platform       string             `json:"platform,omitempty"`
	Dirs           []string           `json:"dirs"`
	FileExtensions []string           `json:"file_extensions,omitempty"`
	Configs        config.ConfigStore `json:"configs"`
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

// Save 把配置写回磁盘（标准 JSON 缩进，原子替换）。
// 注意：这会剥光文件中的所有注释，仅用于无注释可保留的场景（如异常回退）。
// 需要保留注释的全量改写字段，请用 SpliceConfigValues。
func Save(path string, cfg *AppConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	fileMu.Lock()
	defer fileMu.Unlock()
	return writeFileAtomic(path, b, 0o600)
}

// PasswordFieldPointers 返回 cfg 中每个“可能被加密/解密、会发生变化”的密码字段的
// JSON Pointer → 当前值。仅包含非空且非 bcrypt 哈希的字段（bcrypt 哈希从不参与
// 加解密，值不变，无需回写）。供 -encrypt-config/-decrypt-config 用 SpliceConfigValues
// 原位替换这些标量，从而保留文件其余部分的注释与注释掉的远程示例。
func (c *AppConfig) PasswordFieldPointers() map[string]any {
	m := map[string]any{}
	if c.Auth.Password != "" && !IsBcryptHash(c.Auth.Password) {
		m["/auth/password"] = c.Auth.Password
	}
	for name, h := range c.Hosts {
		if h.SSH != nil && h.SSH.Password != "" && !IsBcryptHash(h.SSH.Password) {
			m[fmt.Sprintf("/hosts/%s/ssh/password", escapeJSONPointer(name))] = h.SSH.Password
		}
	}
	return m
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
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.SessionGraceSeconds == 0 {
		c.SessionGraceSeconds = 45
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
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level 非法: %q（可选 debug/info/warn/error）", c.LogLevel)
	}
	if c.SessionGraceSeconds < 5 || c.SessionGraceSeconds > 3600 {
		return fmt.Errorf("session_grace_seconds 非法: %d（应为 5-3600 秒）", c.SessionGraceSeconds)
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
			// 0 表示"用默认值"（applyDefaults 会填成 22/10/30），予以放行；
			// 这里只拦截显式配错的越界值。
			if h.SSH.Port < 0 || h.SSH.Port > 65535 {
				return fmt.Errorf("机器 %q 的 ssh.port 非法: %d（应为 1-65535 或 0 用默认）", name, h.SSH.Port)
			}
			if h.SSH.ConnectTimeoutSeconds < 0 || h.SSH.ConnectTimeoutSeconds > 600 {
				return fmt.Errorf("机器 %q 的 ssh.connect_timeout_seconds 非法: %d（应为 1-600 或 0 用默认）", name, h.SSH.ConnectTimeoutSeconds)
			}
			if h.SSH.KeepAliveSeconds < 0 || h.SSH.KeepAliveSeconds > 3600 {
				return fmt.Errorf("机器 %q 的 ssh.keepalive_seconds 非法: %d（应为 1-3600 或 0 用默认）", name, h.SSH.KeepAliveSeconds)
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
