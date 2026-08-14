package host

import "testing"

func TestRemoteCleanUnix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/var/log", "/var/log"},
		{"/var/log/../log/./nginx", "/var/log/nginx"},
		{"/var/log/", "/var/log"},
		{"//var//log", "/var/log"},
		{".", "."},
		{"rel/../x", "x"},
		{"/a/b/c/../../d", "/a/d"},
		{"/a/../../../x", "/x"},       // rooted: .. 不能越过根
		{"rel/../../x", "../x"},       // 相对路径允许保留 ..
	}
	for _, c := range cases {
		got := remoteClean(c.in, "/")
		if got != c.want {
			t.Errorf("remoteClean(%q, unix) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRemoteCleanWindows(t *testing.T) {
	cases := []struct{ in, want string }{
		{`D:\logs`, `D:\logs`},
		{`D:\logs\..\logs\.\nginx`, `D:\logs\nginx`},
		{`D:/logs/app/../nginx`, `D:\logs\nginx`}, // 正斜杠归一
		{`D:\logs\`, `D:\logs`},
		{`D:\`, `D:\`},
		{`D:`, `D:\`}, // 盘根补齐反斜杠
		{`C:\a\b\..\..\c`, `C:\c`},
		{`\\server\share\..\share\dir`, `\\server\share\share\dir`}, // UNC 下 .. 不能越过 share 根
	}
	for _, c := range cases {
		got := remoteClean(c.in, `\`)
		if got != c.want {
			t.Errorf("remoteClean(%q, windows) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRemoteIsAbs(t *testing.T) {
	unix := "/"
	win := `\`
	abs := []string{"/var/log", "/", `D:\logs`, `D:\`, `\\srv\share`, `C:/x`}
	rel := []string{"var/log", ".", `logs\app`, `D:logs`, `:logs`}
	for _, p := range abs {
		sep := unix
		if looksWindows(p) {
			sep = win
		}
		if !remoteIsAbs(p, sep) {
			t.Errorf("remoteIsAbs(%q,%q) = false, want true", p, sep)
		}
	}
	for _, p := range rel {
		sep := unix
		if looksWindows(p) {
			sep = win
		}
		if remoteIsAbs(p, sep) {
			t.Errorf("remoteIsAbs(%q,%q) = true, want false", p, sep)
		}
	}
}

func looksWindows(p string) bool {
	if len(p) >= 2 && p[1] == ':' {
		return true
	}
	if len(p) >= 2 && (p[0] == '\\' && p[1] == '\\') {
		return true
	}
	return false
}

func TestRemoteWithin(t *testing.T) {
	// Unix 大小写敏感
	if !remoteWithin("/var/log/nginx", "/var/log", "/", false) {
		t.Error("unix: 子目录应当在根内")
	}
	if remoteWithin("/var/log-evil", "/var/log", "/", false) {
		t.Error("unix: 前缀绕过 /var/log-evil 必须被拒")
	}
	if !remoteWithin("/var/log", "/var/log", "/", false) {
		t.Error("unix: 等于根本身应当在根内")
	}
	if remoteWithin("/etc/passwd", "/var/log", "/", false) {
		t.Error("unix: /etc/passwd 不应在 /var/log 内")
	}

	// Windows 大小写不敏感
	if !remoteWithin(`D:\Logs\app.log`, `d:\logs`, `\`, true) {
		t.Error("windows: 大小写不敏感应判为在根内")
	}
	if remoteWithin(`D:\logs-evil\x.log`, `D:\logs`, `\`, true) {
		t.Error("windows: 前缀绕过必须被拒")
	}
}

func TestRemoteParent(t *testing.T) {
	cases := []struct {
		p, sep, parent, base string
	}{
		{"/var/log/nginx/a.log", "/", "/var/log/nginx", "a.log"},
		{"/a.log", "/", "/", "a.log"},
		{`D:\logs\a.log`, `\`, `D:\logs`, "a.log"},
		{`D:\a.log`, `\`, `D:\`, "a.log"},
	}
	for _, c := range cases {
		par, base := remoteParent(c.p, c.sep)
		if par != c.parent || base != c.base {
			t.Errorf("remoteParent(%q) = (%q,%q), want (%q,%q)", c.p, par, base, c.parent, c.base)
		}
	}
}

func TestExtName(t *testing.T) {
	cases := map[string]string{
		"app.log":                ".log",
		"server.out":             ".out",
		"archive.2024.log":       ".log",
		"/var/log/app.log":       ".log",
		`D:\logs\app.LOG`:        ".LOG",
		"noext":                  "",
		"/var/log.dir/file":      "",
	}
	for in, want := range cases {
		if got := extName(in); got != want {
			t.Errorf("extName(%q) = %q, want %q", in, got, want)
		}
	}
}
