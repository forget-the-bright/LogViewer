package cmdbuild

import (
	"strings"
	"testing"
)

// 验证 BuildView 按 platform 选择 sh / powershell，且关键命令片段正确。
// 不做跨平台执行，只做字符串构造层面的断言（CI 在任意 OS 都能跑）。
func TestBuildViewPlatformSelection(t *testing.T) {
	f := FilterCfg{Pattern: "ERROR", CaseSensitive: false}

	for _, platform := range []string{"linux", "darwin"} {
		c := BuildView(platform, "follow", "/var/log/app.log", "utf-8", 200, f)
		if c.Shell != "sh" {
			t.Errorf("%s: want shell sh, got %q", platform, c.Shell)
		}
		if c.Platform != platform {
			t.Errorf("%s: Platform field = %q", platform, c.Platform)
		}
		if !strings.Contains(c.Script, "tail -F -n 200") {
			t.Errorf("%s: 缺少 tail -F -n 200: %s", platform, c.Script)
		}
		if !strings.Contains(c.Script, "grep") {
			t.Errorf("%s: 缺少 grep: %s", platform, c.Script)
		}
	}

	win := BuildView("windows", "follow", `C:\logs\app.log`, "utf-8", 200, f)
	if win.Shell != "powershell" {
		t.Errorf("windows: want shell powershell, got %q", win.Shell)
	}
	if !strings.Contains(win.Script, "Get-Content") {
		t.Errorf("windows: 缺少 Get-Content: %s", win.Script)
	}
	if !strings.Contains(win.Script, "-Wait") {
		t.Errorf("windows follow 缺少 -Wait: %s", win.Script)
	}
	if !strings.Contains(win.Script, "Select-String") {
		t.Errorf("windows: 缺少 Select-String: %s", win.Script)
	}
}

// 未知/空 platform 应走 Unix 分支（保守默认，避免误触发 powershell）。
func TestBuildViewUnknownPlatformDefaultsToUnix(t *testing.T) {
	c := BuildView("", "static", "/tmp/x.log", "utf-8", 0, FilterCfg{})
	if c.Shell != "sh" {
		t.Errorf("空 platform 应回退到 sh，got %q", c.Shell)
	}
}

// BuildExport 等价于 static 模式的 BuildView
func TestBuildExportEqualsStaticView(t *testing.T) {
	f := FilterCfg{Pattern: "ERROR"}
	a := BuildExport("linux", "/f", "utf-8", 100, f)
	b := BuildView("linux", "static", "/f", "utf-8", 100, f)
	if a.Script != b.Script || a.Shell != b.Shell {
		t.Errorf("Export != static View:\nexport=%s\nview=%s", a.Script, b.Script)
	}
}
