package host

import (
	"encoding/base64"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"logviewer/internal/cmdbuild"
)

func decodePS(enc string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		r := rune(binary.LittleEndian.Uint16(b[i : i+2]))
		sb.WriteRune(r)
	}
	return sb.String(), nil
}

func unixViewCmdForTest() cmdbuild.Command {
	return cmdbuild.BuildView("linux", "static", "/var/log/app.log", "utf-8", 100, cmdbuild.FilterCfg{})
}

func windowsViewCmdForTest() cmdbuild.Command {
	return cmdbuild.BuildView("windows", "static", `C:\logs\app.log`, "utf-8", 100, cmdbuild.FilterCfg{})
}

func TestPidFilterReader_StripsMarker(t *testing.T) {
	src := strings.NewReader("LV_PID=12345\nline2\nline3\n")
	pr := &sshProc{pidCh: make(chan struct{})}
	r := newPidFilterReader(src, pr)
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if got != "line2\nline3\n" {
		t.Errorf("got %q, want line2\\nline3\\n", got)
	}
	pid := pr.getPid()
	if pid != 12345 {
		t.Errorf("pid = %d, want 12345", pid)
	}
	select {
	case <-pr.pidCh:
	default:
		t.Error("pidCh should be closed after parsing PID")
	}
}

func TestPidFilterReader_BannerThenMarker(t *testing.T) {
	// sshd banner/MOTD 可能先于 LV_PID 标记出现；必须在预算内继续扫描，
	// 同时把 banner 行原样透传，不能因为第一行不是标记就放弃。
	src := strings.NewReader("Authorized uses only.\nLast login: Mon Aug 17 09:00:00\nLV_PID=12345\nreal output\n")
	pr := &sshProc{pidCh: make(chan struct{})}
	r := newPidFilterReader(src, pr)
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := "Authorized uses only.\nLast login: Mon Aug 17 09:00:00\nreal output\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if pid := pr.getPid(); pid != 12345 {
		t.Errorf("pid = %d, want 12345 (banner 前导时仍需解析到标记)", pid)
	}
	select {
	case <-pr.pidCh:
	default:
		t.Error("pidCh should be closed after parsing PID")
	}
}

func TestPidFilterReader_NoMarkerPassesThrough(t *testing.T) {
	src := strings.NewReader("just some error\n")
	pr := &sshProc{pidCh: make(chan struct{})}
	r := newPidFilterReader(src, pr)
	data, _ := io.ReadAll(r)
	if string(data) != "just some error\n" {
		t.Errorf("got %q", string(data))
	}
	if pr.getPid() != 0 {
		t.Errorf("pid should be 0, got %d", pr.getPid())
	}
}

func TestParseLvPid(t *testing.T) {
	cases := []struct {
		in   string
		pid  int
		ok   bool
	}{
		{"LV_PID=12345", 12345, true},
		{"LV_PID=1\n", 1, true},
		{"  LV_PID=999  ", 999, true},
		{"LV_PID=", 0, false},
		{"LV_PID=abc", 0, false},
		{"some error", 0, false},
		{"LV_PIDX=5", 0, false},
	}
	for _, c := range cases {
		pid, ok := parseLvPid(c.in)
		if pid != c.pid || ok != c.ok {
			t.Errorf("parseLvPid(%q) = (%d,%v), want (%d,%v)", c.in, pid, ok, c.pid, c.ok)
		}
	}
}

func TestBuildRemoteExec_Unix(t *testing.T) {
	cmd := unixViewCmdForTest()
	line := buildRemoteExec(cmd, false)
	if !strings.HasPrefix(line, "sh -c '") {
		t.Errorf("unix exec should start with sh -c ', got: %s", line)
	}
	if !strings.Contains(line, "LV_PID=") {
		t.Errorf("unix exec should contain PID marker, got: %s", line)
	}
	if !strings.Contains(line, "tail -n 100") {
		t.Errorf("unix exec should contain original script, got: %s", line)
	}
}

func TestBuildRemoteExec_Windows(t *testing.T) {
	cmd := windowsViewCmdForTest()

	// 无 pwsh：用 powershell（5.1）
	linePS := buildRemoteExec(cmd, false)
	if !strings.HasPrefix(linePS, "powershell -NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand ") {
		t.Errorf("windows exec (no pwsh) should use powershell, got: %s", linePS)
	}
	enc := strings.TrimPrefix(linePS, "powershell -NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand ")
	decoded, err := decodePS(enc)
	if err != nil {
		t.Fatalf("decode encoded command: %v", err)
	}
	if !strings.Contains(decoded, "LV_PID=") {
		t.Errorf("encoded script should contain PID marker, got: %s", decoded)
	}
	if !strings.Contains(decoded, "Get-Content") {
		t.Errorf("encoded script should contain original Get-Content, got: %s", decoded)
	}

	// 有 pwsh：用 pwsh（7+）
	linePwsh := buildRemoteExec(cmd, true)
	if !strings.HasPrefix(linePwsh, "pwsh -NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand ") {
		t.Errorf("windows exec (pwsh) should use pwsh, got: %s", linePwsh)
	}
}

func TestBuildKillCmd(t *testing.T) {
	unixProc := &sshProc{platform: "linux"}
	unixKill := unixProc.buildKillCmd(4242)
	if !strings.Contains(unixKill, "kill -KILL -4242") {
		t.Errorf("unix kill should target PGID -4242, got: %s", unixKill)
	}
	winProc := &sshProc{platform: "windows"}
	winKill := winProc.buildKillCmd(4242)
	if !strings.Contains(winKill, "taskkill /T /F /PID 4242") {
		t.Errorf("windows kill should use taskkill, got: %s", winKill)
	}
}
