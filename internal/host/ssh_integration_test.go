package host

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"logviewer/internal/appconfig"
	"logviewer/internal/config"
)

// startTestSSHServer 起一个本机回环的 SSH+SFTP 服务，用于集成测试。
// exec "uname -s" 返回 Linux；subsystem sftp 由 pkg/sftp 的默认文件后端提供（直接服务本机文件系统）。
func startTestSSHServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)

	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleTestConn(conn, cfg, done)
		}
	}()

	return ln.Addr().String(), func() {
		ln.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

func handleTestConn(conn net.Conn, cfg *ssh.ServerConfig, done chan struct{}) {
	defer func() {
		conn.Close()
		select {
		case done <- struct{}{}:
		default:
		}
	}()
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			continue
		}
		go handleTestSession(ch, requests)
	}
}

func handleTestSession(ch ssh.Channel, requests <-chan *ssh.Request) {
	for req := range requests {
		switch req.Type {
		case "exec":
			var payload struct{ Cmd string }
			ssh.Unmarshal(req.Payload, &payload)
			_ = req.Reply(true, nil)
			switch strings.TrimSpace(payload.Cmd) {
			case "uname -s":
				// 返回与测试机一致的平台，保证路径分隔符语义与 SFTP 服务的本机 fs 一致。
				switch runtime.GOOS {
				case "linux":
					ch.Write([]byte("Linux"))
				case "darwin":
					ch.Write([]byte("Darwin"))
				default:
					// Windows：uname 不存在，返回非零退出
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 1}))
					_ = ch.Close()
					continue
				}
			case "cmd /c ver":
				if runtime.GOOS == "windows" {
					ch.Write([]byte("\r\nMicrosoft Windows [Version 10.0.26200]\r\n"))
				}
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{}))
			_ = ch.Close()
		case "subsystem":
			var payload struct{ Name string }
			ssh.Unmarshal(req.Payload, &payload)
			if payload.Name == "sftp" {
				_ = req.Reply(true, nil)
				srv, err := sftp.NewServer(ch)
				if err != nil {
					return
				}
				_ = srv.Serve()
			} else {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func newTestSSHHost(t *testing.T, addr, root string) *SSHHost {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	portInt := 0
	for _, c := range port {
		portInt = portInt*10 + int(c-'0')
	}
	h, err := NewSSHHost("testhost", appconfig.SSHConfig{
		Host:                host,
		Port:                portInt,
		Username:            "u",
		Password:            "p",
		InsecureSkipHostKey: true,
		ConnectTimeoutSeconds: 5,
		KeepAliveSeconds:    5,
	}, "", []string{root}, nil, config.NewConfigStore(), nil)
	if err != nil {
		t.Fatalf("NewSSHHost 失败: %v", err)
	}
	return h
}

func TestSSHIntegration_LsStatOpen(t *testing.T) {
	// 本机内嵌 SSH/SFTP 服务在 Windows 上因路径语义与管道关闭时序问题不稳定，
	// 跳过；Windows 开发时用真实远程 Linux 机器手工验证。
	if runtime.GOOS == "windows" {
		t.Skip("skip in-process sftp server test on windows; validate against real remote host")
	}
	addr, cleanup := startTestSSHServer(t)
	defer cleanup()

	tmp := t.TempDir()
	// 准备文件：app.log（含内容）、sub/nested.log、ignore.txt（应被过滤）
	if err := os.WriteFile(filepath.Join(tmp, "app.log"), []byte("hello remote\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tmp, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "sub", "nested.log"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "ignore.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := newTestSSHHost(t, addr, tmp)
	defer h.Close()

	// Ls 会触发首次连接 + 平台探测：应有 app.log 与 sub/，没有 ignore.txt
	nodes, err := h.Ls(tmp)
	if err != nil {
		t.Fatalf("Ls 失败: %v", err)
	}
	var names []string
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	if !contains(names, "app.log") || !contains(names, "sub") {
		t.Errorf("Ls 结果不完整: %v", names)
	}
	if contains(names, "ignore.txt") {
		t.Errorf("Ls 不应包含 .txt: %v", names)
	}

	// 连接建立后平台应已探测为 linux
	if got := h.Platform(); got != "linux" {
		t.Fatalf("Platform = %q, want linux", got)
	}
	if info := h.Info(); !info.Online || info.Platform != "linux" {
		t.Fatalf("Info = %+v, want online/linux", info)
	}

	// Stat
	appLog := filepath.Join(tmp, "app.log")
	fi, err := h.Stat(appLog)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if fi.Size() != int64(len("hello remote\nline2\n")) {
		t.Errorf("Size = %d, want %d", fi.Size(), len("hello remote\nline2\n"))
	}

	// Open 读取
	rc, err := h.Open(appLog)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello remote\nline2\n" {
		t.Errorf("Open 内容 = %q", string(data))
	}

	// 远程命令管道在真实机器上验证；内嵌测试服务器只实现了 uname 与 sftp 子系统，
	// 不具备执行 sh 管道的能力。
}

func TestSSHIntegration_PathTraversal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip in-process sftp server test on windows; validate against real remote host")
	}
	addr, cleanup := startTestSSHServer(t)
	defer cleanup()

	tmp := t.TempDir()
	logFile := filepath.Join(tmp, "a.log")
	if err := os.WriteFile(logFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.log")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := newTestSSHHost(t, addr, tmp)
	defer h.Close()

	// 合法路径
	if _, err := h.ResolvePath(logFile); err != nil {
		t.Errorf("合法路径被拒: %v", err)
	}

	// 词法越界：../../ 应被拒
	bad := filepath.Join(tmp, "..", "..", "etc", "passwd")
	if _, err := h.ResolvePath(bad); err == nil {
		t.Errorf("词法越界路径应被拒: %s", bad)
	}

	// 符号链接逃逸（非 Windows 才测，Windows 创建软链需要管理员）
	if runtime.GOOS != "windows" {
		link := filepath.Join(tmp, "evil.log")
		if err := os.Symlink(outside, link); err == nil {
			_, err := h.ResolvePath(link)
			if err == nil {
				t.Errorf("符号链接逃逸应被拒: %s -> %s", link, outside)
			}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
