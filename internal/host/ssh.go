package host

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"logviewer/internal/appconfig"
	"logviewer/internal/config"
)

// SSHHost 通过 SSH/SFTP 访问远程机器。
//
// 连接是懒建立的：首次 Ls/Stat/Open/ResolvePath 时拨号，失败则下次重试。
// 后台 keepalive 协程定期发 keepalive@openssh.com，失败则断开，下次操作自动重连。
// 主机密钥默认 TOFU：首次连接写入 known_hosts_file，之后严格校验；
// 仅当显式 insecure_skip_host_key=true 才跳过（有日志警告）。
// Capabilities 描述远端具备哪些命令能力，供前端禁用缺失功能（如最小化容器无 iconv/awk）。
type Capabilities struct {
	HasTail  bool `json:"hasTail"`
	HasCat   bool `json:"hasCat"`
	HasGrep  bool `json:"hasGrep"`
	HasAwk   bool `json:"hasAwk"`
	HasIconv bool `json:"hasIconv"`
}

type SSHHost struct {
	name     string
	sshCfg   appconfig.SSHConfig
	dirs     []string // 远端根目录（按配置原样保留，不做本机 Abs）
	platform string   // 配置显式指定的平台（空=自动探测）
	cfgMgr   *config.Manager

	mu            sync.Mutex
	client        *ssh.Client
	sftpClient    *sftp.Client
	realPlatform  string // 探测到的有效平台
	realRoots     []string
	caps          Capabilities
	connected     bool
	lastErr       error
	stopKeepalive chan struct{}
	closeOnce     sync.Once
}

// NewSSHHost 构造一台远程机器 Host。initial/saveCfg 与 LocalHost 含义相同。
func NewSSHHost(name string, sshCfg appconfig.SSHConfig, platform string, dirs []string,
	initial config.ConfigStore, saveCfg config.SaveFunc) (*SSHHost, error) {
	if name == "" {
		return nil, errors.New("机器别名不能为空")
	}
	if sshCfg.Host == "" || sshCfg.Username == "" || sshCfg.Password == "" {
		return nil, fmt.Errorf("机器 %q 的 SSH 配置不完整（需要 host/username/password）", name)
	}
	if sshCfg.Port == 0 {
		sshCfg.Port = 22
	}
	if sshCfg.ConnectTimeoutSeconds == 0 {
		sshCfg.ConnectTimeoutSeconds = 10
	}
	if sshCfg.KeepAliveSeconds == 0 {
		sshCfg.KeepAliveSeconds = 30
	}
	cleanDirs := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d = strings.TrimSpace(d); d != "" {
			cleanDirs = append(cleanDirs, d)
		}
	}
	h := &SSHHost{
		name:     name,
		sshCfg:   sshCfg,
		dirs:     cleanDirs,
		platform: platform,
		cfgMgr:   config.NewManager(initial, saveCfg),
	}
	if len(cleanDirs) == 0 {
		log.Printf("[ssh] %s: 未配置任何扫描目录（dirs），目录树将为空", name)
	}
	// 异步做一次连接探测，让顶栏切换器尽快显示在线状态，不阻塞启动。
	go func() {
		if err := h.ensureConnected(); err != nil {
			log.Printf("[ssh] %s: 初始连接失败: %v", name, err)
		}
	}()
	return h, nil
}

func (h *SSHHost) Name() string             { return h.name }
func (h *SSHHost) Configs() *config.Manager { return h.cfgMgr }

func (h *SSHHost) Dirs() []string {
	out := make([]string, len(h.dirs))
	copy(out, h.dirs)
	return out
}

// Platform 返回有效平台：已连接用探测结果，否则用配置值（可能为空）。
func (h *SSHHost) Platform() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.realPlatform != "" {
		return h.realPlatform
	}
	return h.platform
}

func (h *SSHHost) Info() Info {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.realPlatform
	if p == "" {
		p = h.platform
	}
	msg := ""
	if h.client != nil {
		msg = "在线"
	} else {
		if h.lastErr != nil {
			msg = h.lastErr.Error()
		} else {
			msg = "未连接"
		}
	}
	return Info{
		Name:      h.name,
		Platform:  p,
		Local:     false,
		Online:    h.client != nil,
		Available: h.client != nil,
		Message:   msg,
	}
}

// ---- 连接管理 ----

// ensureConnected 幂等：已连接直接返回，否则拨号。
func (h *SSHHost) ensureConnected() error {
	h.mu.Lock()
	if h.client != nil {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	cb, err := h.hostKeyCallback()
	if err != nil {
		return h.recordErr(err)
	}

	timeout := time.Duration(h.sshCfg.ConnectTimeoutSeconds) * time.Second
	client, err := ssh.Dial("tcp",
		net.JoinHostPort(h.sshCfg.Host, strconv.Itoa(h.sshCfg.Port)),
		&ssh.ClientConfig{
			User:            h.sshCfg.Username,
			Auth:            []ssh.AuthMethod{ssh.Password(h.sshCfg.Password)},
			HostKeyCallback: cb,
			Timeout:         timeout,
		})
	if err != nil {
		return h.recordErr(fmt.Errorf("SSH 连接 %s@%s:%d 失败: %w",
			h.sshCfg.Username, h.sshCfg.Host, h.sshCfg.Port, err))
	}

	sc, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return h.recordErr(fmt.Errorf("SFTP 子系统不可用: %w（请确认远端 sshd_config 启用了 Subsystem sftp）", err))
	}

	platform := h.platform
	if platform == "" {
		platform, err = detectPlatform(client)
		if err != nil {
			sc.Close()
			client.Close()
			return h.recordErr(fmt.Errorf("平台探测失败: %w", err))
		}
	}

	// 预解析根目录真实路径（用于符号链接逃逸校验）。
	realRoots := make([]string, len(h.dirs))
	sep := remoteSep(platform)
	for i, d := range h.dirs {
		cleaned := remoteClean(d, sep)
		if rp, err := sc.RealPath(cleaned); err == nil {
			realRoots[i] = remoteClean(rp, sep)
		} else {
			// 根目录可能暂不存在（跟踪场景），用词法清洗后的路径兜底。
			realRoots[i] = cleaned
		}
	}

	// 探测远端命令能力（最小化容器可能缺 iconv/awk）。Windows 远程用 PowerShell，
	// 不依赖这些 Unix 工具，跳过探测直接标 true。
	caps := detectCapabilities(client, platform)

	h.mu.Lock()
	h.client = client
	h.sftpClient = sc
	h.realPlatform = platform
	h.realRoots = realRoots
	h.caps = caps
	h.connected = true
	h.lastErr = nil
	stop := make(chan struct{})
	h.stopKeepalive = stop
	h.mu.Unlock()

	log.Printf("[ssh] %s: 已连接（平台=%s，tail=%v cat=%v grep=%v awk=%v iconv=%v）",
		h.name, platform, caps.HasTail, caps.HasCat, caps.HasGrep, caps.HasAwk, caps.HasIconv)
	if !caps.HasIconv || !caps.HasAwk {
		log.Printf("[ssh] %s: 远端缺少部分命令（GBK 转码需 iconv，时间过滤需 awk），相关功能将被禁用", h.name)
	}
	go h.keepalive(stop)
	return nil
}

// detectCapabilities 探测远端是否具备所需的 Unix 命令。Windows 远程恒为 true（用 PowerShell）。
func detectCapabilities(client *ssh.Client, platform string) Capabilities {
	if platform == "windows" {
		return Capabilities{HasTail: true, HasCat: true, HasGrep: true, HasAwk: true, HasIconv: true}
	}
	// 一条命令批量检查：`command -v` 在 sh/bash/dash/zsh 都可用；
	// 对每个命令输出 "name=1" 或 "name=0"。
	probe := `for c in tail cat grep awk iconv; do ` +
		`if command -v $c >/dev/null 2>&1; then echo "${c}=1"; else echo "${c}=0"; fi; done`
	out, err := runRemote(client, probe)
	caps := Capabilities{}
	if err != nil {
		// 探测失败时保守地全部标 true，不让探测异常阻塞使用（命令真缺失时运行时会报错）。
		caps.HasTail, caps.HasCat, caps.HasGrep, caps.HasAwk, caps.HasIconv = true, true, true, true, true
		return caps
	}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 {
			continue
		}
		v := parts[1] == "1"
		switch parts[0] {
		case "tail":
			caps.HasTail = v
		case "cat":
			caps.HasCat = v
		case "grep":
			caps.HasGrep = v
		case "awk":
			caps.HasAwk = v
		case "iconv":
			caps.HasIconv = v
		}
	}
	return caps
}

// Capabilities 返回远端命令能力快照。
func (h *SSHHost) Capabilities() Capabilities {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.caps
}

func (h *SSHHost) recordErr(err error) error {
	h.mu.Lock()
	h.lastErr = err
	h.mu.Unlock()
	return err
}

// keepalive 定期发保活请求；失败则断开，下次操作自动重连。
func (h *SSHHost) keepalive(stop chan struct{}) {
	d := time.Duration(h.sshCfg.KeepAliveSeconds) * time.Second
	if d <= 0 {
		d = 30 * time.Second
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			h.mu.Lock()
			client := h.client
			h.mu.Unlock()
			if client == nil {
				return
			}
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				log.Printf("[ssh] %s: keepalive 失败，将重连: %v", h.name, err)
				h.closeConn()
				return
			}
		}
	}
}

// closeConn 断开当前连接（可被 keepalive 失败或重试用）。
func (h *SSHHost) closeConn() {
	h.mu.Lock()
	if h.stopKeepalive != nil {
		close(h.stopKeepalive)
		h.stopKeepalive = nil
	}
	sc := h.sftpClient
	cl := h.client
	h.sftpClient = nil
	h.client = nil
	h.connected = false
	h.mu.Unlock()
	if sc != nil {
		_ = sc.Close()
	}
	if cl != nil {
		_ = cl.Close()
	}
}

// Close 释放资源（进程退出时调用）。
func (h *SSHHost) Close() error {
	h.closeOnce.Do(h.closeConn)
	return nil
}

// withSFTP 拿到 sftp client 执行 fn；遇到连接错误自动重连一次。
func (h *SSHHost) withSFTP(fn func(*sftp.Client) error) error {
	if err := h.ensureConnected(); err != nil {
		return err
	}
	h.mu.Lock()
	sc := h.sftpClient
	h.mu.Unlock()
	if sc == nil {
		return errors.New("SFTP 未连接")
	}
	err := fn(sc)
	if err != nil && isConnErr(err) {
		log.Printf("[ssh] %s: 操作失败，断开重连一次: %v", h.name, err)
		h.closeConn()
		if err2 := h.ensureConnected(); err2 != nil {
			return err // 返回原始错误
		}
		h.mu.Lock()
		sc = h.sftpClient
		h.mu.Unlock()
		err = fn(sc)
	}
	return err
}

func isConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{"connection", "closed", "i/o timeout", "broken pipe",
		"ssh: disconnect", "client session disconnected", "no common algorithm"} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}

// ---- 平台探测 ----

// detectPlatform 在远端执行 uname / ver 判断操作系统。
// 不依赖登录 shell 的种类：优先 sh 风格的 uname，失败再试 Windows 的 cmd /c ver。
func detectPlatform(client *ssh.Client) (string, error) {
	if out, err := runRemote(client, "uname -s"); err == nil {
		switch strings.TrimSpace(string(out)) {
		case "Linux":
			return "linux", nil
		case "Darwin":
			return "darwin", nil
		}
	}
	if out, err := runRemote(client, "cmd /c ver"); err == nil {
		if strings.Contains(strings.ToLower(string(out)), "windows") {
			return "windows", nil
		}
	}
	return "", errors.New("uname 与 ver 均未识别出平台（远端可能是不支持的系统）")
}

// runRemote 在远端执行一条命令并返回 stdout。
func runRemote(client *ssh.Client, cmdline string) ([]byte, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return sess.Output(cmdline)
}

// ---- 主机密钥（known_hosts TOFU）----

func (h *SSHHost) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if h.sshCfg.InsecureSkipHostKey {
		log.Printf("[ssh] %s: 警告！insecure_skip_host_key=true，已跳过主机密钥校验，存在中间人风险", h.name)
		return ssh.InsecureIgnoreHostKey(), nil
	}

	khFile := h.sshCfg.KnownHostsFile
	if khFile == "" {
		if home, err := os.UserHomeDir(); err == nil {
			khFile = filepath.Join(home, ".ssh", "known_hosts")
		}
	} else {
		khFile = expandHome(khFile)
	}
	if khFile == "" {
		return nil, errors.New("无法确定 known_hosts 文件路径")
	}

	cb, err := knownhosts.New(khFile)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("加载 known_hosts %s 失败: %w", khFile, err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if cb != nil {
			err := cb(hostname, remote, key)
			if err == nil {
				return nil
			}
			var ke *knownhosts.KeyError
			if errors.As(err, &ke) {
				if len(ke.Want) > 0 {
					// 主机已存在但密钥不匹配 → 可能中间人，拒绝连接。
					return fmt.Errorf("主机 %s 密钥已变更（疑似中间人攻击）: %w；如确认可信，请手动从 %s 删除旧记录",
						h.sshCfg.Host, err, khFile)
				}
				// Want 为空 → 未知主机，走 TOFU 落盘
			} else {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(khFile), 0o700); err != nil {
			return fmt.Errorf("创建 .ssh 目录失败: %w", err)
		}
		addr := knownhosts.Normalize(net.JoinHostPort(h.sshCfg.Host, strconv.Itoa(h.sshCfg.Port)))
		line := knownhosts.Line([]string{addr}, key)
		f, err := os.OpenFile(khFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("写入 known_hosts 失败: %w", err)
		}
		defer f.Close()
		if _, err := f.WriteString(line + "\n"); err != nil {
			return fmt.Errorf("写入 known_hosts 失败: %w", err)
		}
		log.Printf("[ssh] %s: 首次连接，主机密钥已写入 %s", h.name, khFile)
		return nil
	}, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

