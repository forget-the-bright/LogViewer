package host

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/procmgr"
)

// sshProc 把一条远程命令包装成 procmgr.Process。
//
// 关键设计：
//   - stdout/stderr 分别通过 SSH session 的 pipe 获取（不用 PTY，因为 PTY 会合并两条流
//     且做 ONLCR 转换，影响日志字节与错误分离）。
//   - 远程 shell 启动时先向 stderr 打印一行 "LV_PID=<pid>"，sshProc 拦截首行解析出 PID，
//     用于 Kill 时杀整个进程组（Unix: kill -KILL -<pid>；Windows: taskkill /T /F /PID）。
//   - Unix 下非交互 sh 不为管道创建新进程组，所有子进程（tail/grep/awk）都与 sh 同 PGID，
//     因此 kill -KILL -<pid> 能连同整条管道一起终止，不会留孤儿。
//   - Windows OpenSSH 对信号支持差，Kill 时另开一个 SSH session 执行 taskkill。
type sshProc struct {
	host     *SSHHost
	platform string
	cmdLine  string // 最终通过 SSH exec 发送的命令行

	session *ssh.Session
	stdout  io.Reader
	stderr  io.Reader // 经过 pidFilterReader 过滤后的 stderr

	pidMu      sync.Mutex
	remotePid  int
	pidCh      chan struct{} // PID 解析成功后关闭
	startOnce  sync.Once
	waitErr    error
	waitOnce   sync.Once
	killCalled bool
}

// buildRemoteExec 把 cmdbuild.Command 翻译成要通过 SSH exec 发送的命令行。
// 同时在脚本前注入 PID 标记，供 Kill 精确定位远程进程组。
func buildRemoteExec(cmd cmdbuild.Command) string {
	if cmd.Shell == "powershell" {
		// PowerShell：在脚本前加一行把 $PID 打到 stderr，然后执行原脚本。
		// 用 -EncodedCommand（UTF-16LE base64）完全绕过 cmd.exe 的引号转义问题。
		wrapped := "[Console]::Error.WriteLine('LV_PID=' + $PID)\n" +
			"[Console]::Error.Flush()\n" +
			"$ErrorActionPreference='Continue'\n" +
			// 静默进度记录，避免 PowerShell 把"正在准备首次使用模块"等进度以 CLIXML 写入 stderr，污染前端错误提示。
			"$ProgressPreference='SilentlyContinue'\n" +
			cmd.Script
		return "powershell -NoProfile -NonInteractive -EncodedCommand " + encodePS(wrapped)
	}
	// Unix：用 sh -c 执行。先 echo PID 到 stderr，再运行原脚本。
	// 用 printf 避免 echo 在不同 shell 下的差异；单引号转义复用 cmdbuild.shQuote 风格。
	wrapped := "printf 'LV_PID=%s\\n' \"$$\" >&2; " + cmd.Script
	return "sh -c " + shSingleQuote(wrapped)
}

// encodePS 把 PowerShell 脚本编码为 -EncodedCommand 所需的 UTF-16LE base64。
func encodePS(s string) string {
	// Go 的字符串是 UTF-8；手动转 UTF-16LE。
	u16 := utf16Encode(s)
	return base64.StdEncoding.EncodeToString(u16)
}

// utf16Encode 把 UTF-8 字符串编码为 UTF-16LE 字节（不处理 BMP 外字符，日志脚本不会遇到）。
func utf16Encode(s string) []byte {
	buf := bytes.Buffer{}
	for _, r := range s {
		if r < 0x10000 {
			buf.WriteByte(byte(r))
			buf.WriteByte(byte(r >> 8))
		} else {
			// 代理对（极少出现在命令脚本里）
			r -= 0x10000
			lo := 0xD800 + (r >> 10)
			hi := 0xDC00 + (r & 0x3FF)
			buf.WriteByte(byte(lo))
			buf.WriteByte(byte(lo >> 8))
			buf.WriteByte(byte(hi))
			buf.WriteByte(byte(hi >> 8))
		}
	}
	return buf.Bytes()
}

// shSingleQuote 用 POSIX 单引号包裹字符串（与 cmdbuild.shQuote 相同，这里独立避免循环引用暴露）。
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newSSHProc 构造远程进程（尚未启动）。
func (h *SSHHost) newProc(cmd cmdbuild.Command) (*sshProc, error) {
	if err := h.ensureConnected(); err != nil {
		return nil, err
	}
	platform := h.Platform()
	return &sshProc{
		host:     h,
		platform: platform,
		cmdLine:  buildRemoteExec(cmd),
		pidCh:    make(chan struct{}),
	}, nil
}

// Run 在远程机器上执行命令管道，返回可被 procmgr 管控的进程。
func (h *SSHHost) Run(cmd cmdbuild.Command) (procmgr.Process, error) {
	return h.newProc(cmd)
}

func (p *sshProc) StdoutPipe() (io.Reader, error) {
	sess, err := p.host.newSession()
	if err != nil {
		return nil, err
	}
	p.session = sess
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	p.stdout = stdout
	p.stderr = newPidFilterReader(stderr, p)
	return p.stdout, nil
}

func (p *sshProc) StderrPipe() (io.Reader, error) {
	if p.session == nil {
		return nil, errors.New("必须先调用 StdoutPipe")
	}
	return p.stderr, nil
}

func (p *sshProc) Start() error {
	if p.session == nil {
		return errors.New("必须先调用 StdoutPipe")
	}
	var err error
	p.startOnce.Do(func() {
		err = p.session.Start(p.cmdLine)
	})
	return err
}

func (p *sshProc) Wait() error {
	p.waitOnce.Do(func() {
		if p.session != nil {
			p.waitErr = p.session.Wait()
		}
	})
	// SSH session 被 Close 时会返回 *ssh.ExitMissingError 或 io.EOF，视为正常终止。
	// 被信号杀死的进程会返回 *ssh.ExitError（非零退出），这也属于预期内的停止，不报错。
	if p.waitErr != nil {
		if errors.Is(p.waitErr, io.EOF) {
			return nil
		}
		var emErr *ssh.ExitMissingError
		if errors.As(p.waitErr, &emErr) {
			return nil
		}
		var exitErr *ssh.ExitError
		if errors.As(p.waitErr, &exitErr) {
			return nil
		}
	}
	return p.waitErr
}

// Kill 终止远程进程树。
func (p *sshProc) Kill() error {
	p.pidMu.Lock()
	p.killCalled = true
	p.pidMu.Unlock()

	if p.platform == "windows" {
		// Windows OpenSSH：仅关闭会话不能让 Get-Content -Wait 的 PowerShell 立即退出，
		// stdout 管道可能要 ~1s 才 EOF（sshd 等待作业对象全部退出）。
		// 先同步 taskkill 进程树（/T 连带子孙），管道随进程死亡关闭；再关 session 回收通道。
		// 前端停止按钮会立即进入"停止中"loading 态，不阻塞用户操作。
		if pid := p.getPid(); pid > 0 {
			_ = p.killRemoteTree(pid)
		}
		if p.session != nil {
			_ = p.session.Close()
		}
		return nil
	}
	// Unix：关闭 SSH 会话会让 sh 收到 SIGHUP，整条管道立即退出，stdout 随即 EOF（实测 ~25ms）。
	// 再异步 kill -KILL 进程组兜底，错误忽略。
	if p.session != nil {
		_ = p.session.Close()
	}
	if pid := p.getPid(); pid > 0 {
		go func() { _ = p.killRemoteTree(pid) }()
	}
	return nil
}

// killRemoteTree 在远端执行杀进程树命令。
func (p *sshProc) killRemoteTree(pid int) error {
	killCmd := p.buildKillCmd(pid)
	return p.host.runOneShot(killCmd)
}

func (p *sshProc) buildKillCmd(pid int) string {
	if p.platform == "windows" {
		// 直接用 taskkill 终止整个进程树。不套 powershell -Command：PowerShell 启动本身
		// 就要几百毫秒，是停止慢的主要原因；taskkill 在 cmd / PowerShell 默认 shell 下都能直接执行。
		return fmt.Sprintf("taskkill /T /F /PID %d", pid)
	}
	// Unix：直接 KILL 整个进程组（-pid 表示 PGID），sh 启动管道时所有子进程
	// （tail/grep/iconv/awk）都与 sh 同 PGID（非交互 shell 不启用作业控制）。
	// 日志读取进程无需优雅退出，直接 KILL 最快最稳，避免 sleep 0.2 阻塞停止响应。
	return fmt.Sprintf("kill -KILL -%d 2>/dev/null; kill -KILL %d 2>/dev/null", pid, pid)
}

// runOneShot 另开一个 SSH session 执行短命令（用于杀进程）。
func (h *SSHHost) runOneShot(cmdline string) error {
	h.mu.Lock()
	client := h.client
	h.mu.Unlock()
	if client == nil {
		return errors.New("SSH 未连接")
	}
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	_ = sess.Run(cmdline)
	return nil
}

// RunOneShot 在远端执行一条短命命令至结束，返回合并输出与退出码。
// 用于正则语法校验等一次性空跑检查。命令自身的非零退出通过 exitCode 返回，
// 不视为 Go error；仅会话/连接级错误才返回 err。
func (h *SSHHost) RunOneShot(cmd cmdbuild.Command) (string, int, error) {
	if err := h.ensureConnected(); err != nil {
		return "", -1, err
	}
	h.mu.Lock()
	client := h.client
	h.mu.Unlock()
	if client == nil {
		return "", -1, errors.New("SSH 未连接")
	}
	sess, err := client.NewSession()
	if err != nil && isConnErr(err) {
		// 连接错误：仅当旧连接仍是当前连接时才拆除（避免误杀并发重连建好的新连接），
		// 然后重连一次重试。
		h.invalidateConn(client)
		if err2 := h.ensureConnected(); err2 != nil {
			return "", -1, err2
		}
		h.mu.Lock()
		client = h.client
		h.mu.Unlock()
		if client == nil {
			return "", -1, errors.New("SSH 未连接")
		}
		sess, err = client.NewSession()
	}
	if err != nil {
		return "", -1, err
	}
	defer sess.Close()

	// 一次性短命令不需要 PID 标记/进程树管控，直接按 shell 执行即可，
	// 避免 LV_PID 标记行污染校验输出。
	cmdline := oneShotCmdLine(cmd)
	out, runErr := sess.CombinedOutput(cmdline)
	if runErr == nil {
		return string(out), 0, nil
	}
	var exitErr *ssh.ExitError
	if errors.As(runErr, &exitErr) {
		return string(out), exitErr.ExitStatus(), nil
	}
	return string(out), -1, runErr
}

// oneShotCmdLine 把 cmdbuild.Command 翻译成一次性执行的命令行（不注入 PID 标记）。
func oneShotCmdLine(cmd cmdbuild.Command) string {
	if cmd.Shell == "powershell" {
		return "powershell -NoProfile -NonInteractive -EncodedCommand " + encodePS(cmd.Script)
	}
	return "sh -c " + shSingleQuote(cmd.Script)
}

// newSession 从当前 client 创建一个新 session（供 StdoutPipe 使用）。
// 如果连接已断开，尝试重连后再创建。重连用 invalidateConn(old) 做比较拆除，
// 避免并发在途操作误杀彼此已重建的连接。
func (h *SSHHost) newSession() (*ssh.Session, error) {
	h.mu.Lock()
	client := h.client
	h.mu.Unlock()
	if client == nil {
		// 连接已断开，触发重连
		if err := h.ensureConnected(); err != nil {
			return nil, err
		}
		h.mu.Lock()
		client = h.client
		h.mu.Unlock()
		if client == nil {
			return nil, errors.New("SSH 未连接")
		}
	}
	sess, err := client.NewSession()
	if err != nil && isConnErr(err) {
		// 会话创建失败且为连接错误：仅当旧连接仍是当前连接时拆除，再重连重试一次。
		h.invalidateConn(client)
		if err2 := h.ensureConnected(); err2 != nil {
			return nil, err2
		}
		h.mu.Lock()
		client = h.client
		h.mu.Unlock()
		if client == nil {
			return nil, errors.New("SSH 未连接")
		}
		return client.NewSession()
	}
	return sess, err
}

// setPid 由 pidFilterReader 在解析到 PID 时调用。
func (p *sshProc) setPid(pid int) {
	p.pidMu.Lock()
	if p.remotePid == 0 && pid > 0 {
		p.remotePid = pid
		close(p.pidCh)
	}
	p.pidMu.Unlock()
}

func (p *sshProc) getPid() int {
	p.pidMu.Lock()
	defer p.pidMu.Unlock()
	return p.remotePid
}

// pidFilterReader 从 stderr 读取并拦截第一行 "LV_PID=<pid>"，其余原样透传。
// 远程命令一启动就会打印这行（在管道输出之前），所以用 bufio 逐行扫描是安全的。
type pidFilterReader struct {
	src    *bufio.Reader
	proc   *sshProc
	passed bool   // 是否已处理完首行
	buf    []byte // 透传缓冲
}

func newPidFilterReader(r io.Reader, proc *sshProc) *pidFilterReader {
	return &pidFilterReader{src: bufio.NewReader(r), proc: proc}
}

func (pr *pidFilterReader) Read(p []byte) (int, error) {
	if len(pr.buf) > 0 {
		n := copy(p, pr.buf)
		pr.buf = pr.buf[n:]
		return n, nil
	}
	line, err := pr.src.ReadString('\n')
	if line == "" && err != nil {
		return 0, err
	}
	if !pr.passed {
		pr.passed = true
		if pid, ok := parseLvPid(line); ok {
			pr.proc.setPid(pid)
			// 消费掉这行，不向外透传
			if err != nil {
				return 0, err
			}
			return pr.Read(p) // 递归读下一行
		}
	}
	// 非标记行：透传
	n := copy(p, line)
	if n < len(line) {
		pr.buf = []byte(line[n:])
	}
	if n > 0 {
		return n, nil
	}
	return 0, err
}

func parseLvPid(line string) (int, bool) {
	line = strings.TrimSpace(line)
	const prefix = "LV_PID="
	if !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	s := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return pid, true
}

