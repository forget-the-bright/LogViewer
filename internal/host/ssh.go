package host

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
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

// knownHostsMu 串行化对 known_hosts 文件的 TOFU 追加写入，
// 防止多个 SSHHost 首次并发连接时 O_APPEND 写入交错导致文件损坏。
var knownHostsMu sync.Mutex

// Capabilities 描述远端具备哪些原生命令，供前端禁用缺失功能
// （如最小化容器无 iconv/awk）。本机恒为全部 true。
type Capabilities struct {
	HasTail  bool `json:"hasTail"`
	HasCat   bool `json:"hasCat"`
	HasGrep  bool `json:"hasGrep"`
	HasAwk   bool `json:"hasAwk"`
	HasIconv bool `json:"hasIconv"`
}

// SSHHost 通过 SSH/SFTP 访问远程机器。
//
// 连接是懒建立的：首次 Ls/Stat/Open/ResolvePath 时拨号，失败则下次重试。
// 后台 keepalive 协程定期发 keepalive@openssh.com，失败则断开，下次操作自动重连。
// 主机密钥默认 TOFU：首次连接写入 known_hosts_file，之后严格校验；
// 仅当显式 insecure_skip_host_key=true 才跳过（有日志警告）。
type SSHHost struct {
	name     string
	sshCfg   appconfig.SSHConfig
	dirs     []string // 远端根目录（按配置原样保留，不做本机 Abs）
	platform string   // 配置显式指定的平台（空=自动探测）
	exts     map[string]bool
	showAll  bool // true 时展示所有文件（file_extensions 含 "*"）
	cfgMgr   *config.Manager

	mu            sync.Mutex
	reconnMu      sync.Mutex // 串行化重连，防止并发失败操作触发多次拨号
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
// exts 控制目录树展示的文件后缀（nil=默认 .log/.out，含 "*" 展示全部）。
func NewSSHHost(name string, sshCfg appconfig.SSHConfig, platform string, dirs []string,
	exts []string, initial config.ConfigStore, saveCfg config.SaveFunc) (*SSHHost, error) {
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
	extSet, showAll := normalizeExts(exts)
	h := &SSHHost{
		name:     name,
		sshCfg:   sshCfg,
		dirs:     cleanDirs,
		platform: platform,
		exts:     extSet,
		showAll:  showAll,
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
	p := h.realPlatform
	if p == "" {
		p = h.platform
	}
	online := h.client != nil
	var errMsg string
	if h.lastErr != nil {
		errMsg = h.lastErr.Error()
	}
	h.mu.Unlock()

	msg := "未连接"
	if online {
		msg = "在线"
	} else if errMsg != "" {
		msg = errMsg
	}
	return Info{
		Name:      h.name,
		Platform:  p,
		Local:     false,
		Online:    online,
		Available: online,
		Message:   msg,
	}
}

// Fingerprint 返回主机的连接配置指纹，用于 Rebuild 时判断配置是否变更。
// 任何影响连接/行为/可见范围的字段都必须计入，否则热加载会静默保留旧实例：
// 不仅是 host/port/user/password，还包括 dirs（可见目录）、platform（命令平台）、
// 超时与保活参数。
func (h *SSHHost) Fingerprint() string {
	// 后缀集合计入指纹，使 file_extensions 改动触发热加载替换实例。
	var extToken string
	if h.showAll {
		extToken = "*"
	} else {
		exts := make([]string, 0, len(h.exts))
		for e := range h.exts {
			exts = append(exts, e)
		}
		sort.Strings(exts)
		extToken = strings.Join(exts, ",")
	}
	return fmt.Sprintf("%s|%s:%d|%s|%s|%s|%v|plat=%s|dirs=%s|exts=%s|to=%d|ka=%d",
		h.name, h.sshCfg.Host, h.sshCfg.Port, h.sshCfg.Username,
		h.sshCfg.Password, h.sshCfg.KnownHostsFile, h.sshCfg.InsecureSkipHostKey,
		h.platform, strings.Join(h.dirs, ","), extToken,
		h.sshCfg.ConnectTimeoutSeconds, h.sshCfg.KeepAliveSeconds)
}

// ---- 连接管理 ----

// ensureConnected 幂等：已连接直接返回，否则拨号。
// 使用 reconnMu 串行化重连，防止并发失败操作触发多次拨号。
func (h *SSHHost) ensureConnected() error {
	h.mu.Lock()
	if h.client != nil {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	h.reconnMu.Lock()
	defer h.reconnMu.Unlock()

	// Double-check：在等锁期间可能已有其他 goroutine 连接成功
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

	// SSH 握手已由 ssh.Dial 的 Timeout 控制，但 SFTP 子系统协商、平台探测、
	// 能力探测是握手后的多次往返，异常/慢速服务器可能让它们无限阻塞。
	// 这里用同一个连接超时把这一整段初始化包起来；超时后关闭 client，
	// 让仍在阻塞的子请求随连接关闭而返回。
	type initResult struct {
		sc        *sftp.Client
		platform  string
		realRoots []string
		caps      Capabilities
		err       error
	}
	initDone := make(chan initResult, 1)
	go func() {
		sc, err := sftp.NewClient(client)
		if err != nil {
			initDone <- initResult{err: fmt.Errorf("SFTP 子系统不可用: %w（请确认远端 sshd_config 启用了 Subsystem sftp）", err)}
			return
		}
		platform := h.platform
		if platform == "" {
			platform, err = detectPlatform(client)
			if err != nil {
				initDone <- initResult{sc: sc, err: fmt.Errorf("平台探测失败: %w", err)}
				return
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
		caps := detectCapabilities(client, platform)
		initDone <- initResult{sc: sc, platform: platform, realRoots: realRoots, caps: caps}
	}()

	var res initResult
	select {
	case res = <-initDone:
	case <-time.After(timeout):
		client.Close()
		// 等待 goroutine 因连接关闭而返回；最多再等 2s，避免极端情况下永久阻塞。
		select {
		case <-initDone:
		case <-time.After(2 * time.Second):
			log.Printf("[ssh] %s: 初始化超时后后台清理未在 2s 内返回，放弃等待", h.name)
		}
		return h.recordErr(fmt.Errorf("SSH 初始化超时（SFTP/平台/能力探测超过 %s）", timeout))
	}
	if res.err != nil {
		if res.sc != nil {
			res.sc.Close()
		}
		client.Close()
		return h.recordErr(res.err)
	}
	sc, platform, realRoots, caps := res.sc, res.platform, res.realRoots, res.caps

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
				// 用比较拆除：仅当当前连接仍是这个失败的 client 时才关闭。
				// 否则可能有在途操作已触发重连、换上了新连接，不能误杀。
				h.invalidateConn(client)
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

// invalidateConn 仅当当前存储的连接仍是 old 时才断开并清空。
//
// 并发安全问题：多个在途操作可能同时拿到同一个 client/sftp，当其中一个遇到连接
// 错误时，若直接调用 closeConn()，会把"另一个 goroutine 已经触发重连、刚建好的
// 新连接"也一起关掉，造成新连接被误杀、重试抖动。传入"失败时持有的那个旧连接"，
// 只在它仍是当前连接时才关闭，已被替换则什么都不做。
func (h *SSHHost) invalidateConn(old *ssh.Client) {
	h.mu.Lock()
	if h.client != old {
		// 已被其他 goroutine 重连替换，不要误杀新连接。
		h.mu.Unlock()
		return
	}
	if h.stopKeepalive != nil {
		close(h.stopKeepalive)
		h.stopKeepalive = nil
	}
	sc := h.sftpClient
	h.sftpClient = nil
	h.client = nil
	h.connected = false
	h.mu.Unlock()
	if sc != nil {
		_ = sc.Close()
	}
	if old != nil {
		_ = old.Close()
	}
}

// Close 释放资源（进程退出时调用）。
func (h *SSHHost) Close() error {
	h.closeOnce.Do(h.closeConn)
	return nil
}

// withSFTP 拿到 sftp client 执行 fn；遇到连接错误自动重连一次。
//
// 重连用 invalidateConn(old) 而非无条件 closeConn：并发在途操作中，只有
// "失败时仍持有旧连接"的那个 goroutine 负责拆除，避免误杀别人建好的新连接。
func (h *SSHHost) withSFTP(fn func(*sftp.Client) error) error {
	if err := h.ensureConnected(); err != nil {
		return err
	}
	h.mu.Lock()
	sc := h.sftpClient
	cl := h.client
	h.mu.Unlock()
	if sc == nil {
		return errors.New("SFTP 未连接")
	}
	err := fn(sc)
	if err != nil && isConnErr(err) {
		log.Printf("[ssh] %s: SFTP 操作失败，断开重连一次: %v", h.name, err)
		h.invalidateConn(cl)
		if err2 := h.ensureConnected(); err2 != nil {
			return err // 重连失败返回原始连接错误
		}
		h.mu.Lock()
		sc = h.sftpClient
		h.mu.Unlock()
		if sc == nil {
			return err
		}
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

// detectPlatform 在远端执行探测命令判断操作系统。
//
// 探测顺序不依赖登录 shell 种类：
//   - uname -s：Linux/macOS 及带 uname 的环境（Git Bash/WSL）。
//   - ver：Win32-OpenSSH 默认 shell 是 cmd.exe，裸 `ver` 是其内建命令，
//     可正常输出 "Microsoft Windows [版本 ...]"。注意不能用 `cmd /c ver`：
//     sshd 已用默认 shell 包了一层，再嵌套 `cmd /c ver` 会触发 cmd 的引号
//     处理 bug，报 `'ver"' 不是内部或外部命令`（已在真实 Windows OpenSSH 上验证）。
//   - powershell 兜底：若默认 shell 被改成 PowerShell（裸 ver 不可用），
//     用 .NET 查询平台标识，"Win32NT" 即 Windows。
//
// Windows 的 ver 输出在中文系统下是 GBK 编码，但 "Windows" 是 ASCII 字节，
// 用大小写不敏感的子串匹配即可跨代码页稳定命中。
func detectPlatform(client *ssh.Client) (string, error) {
	if out, err := runRemote(client, "uname -s"); err == nil {
		switch strings.TrimSpace(string(out)) {
		case "Linux":
			return "linux", nil
		case "Darwin":
			return "darwin", nil
		}
	}
	if out, err := runRemote(client, "ver"); err == nil {
		if isWindowsOutput(out) {
			return "windows", nil
		}
	}
	if out, err := runRemote(client, `powershell -NoProfile -NonInteractive -Command "[Environment]::OSVersion.Platform]"`); err == nil {
		if strings.Contains(strings.ToLower(string(out)), "win32nt") || isWindowsOutput(out) {
			return "windows", nil
		}
	}
	return "", errors.New("uname/ver/powershell 均未识别出平台（远端可能是不支持的系统）")
}

// isWindowsOutput 判断命令输出是否为 Windows 标识。"windows" 为 ASCII，
// 即使远端输出是 GBK/UTF-16 也能稳定匹配到该子串。
func isWindowsOutput(out []byte) bool {
	return strings.Contains(strings.ToLower(string(out)), "windows")
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
		addr := knownhosts.Normalize(net.JoinHostPort(h.sshCfg.Host, strconv.Itoa(h.sshCfg.Port)))
		line := knownhosts.Line([]string{addr}, key)
		// 加锁串行化追加写入：不同主机可能共用同一个 known_hosts 文件，
		// 且同一主机的并发握手也可能同时触发 TOFU。
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()
		if err := os.MkdirAll(filepath.Dir(khFile), 0o700); err != nil {
			return fmt.Errorf("创建 .ssh 目录失败: %w", err)
		}
		f, err := os.OpenFile(khFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("写入 known_hosts 失败: %w", err)
		}
		_, writeErr := f.WriteString(line + "\n")
		closeErr := f.Close()
		if writeErr != nil {
			return fmt.Errorf("写入 known_hosts 失败: %w", writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("写入 known_hosts 失败: %w", closeErr)
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
