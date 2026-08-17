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

// 验证 Windows 编码分支构造正确。这是 GBK 性能与跨版本/跨区域正确性的回归保护：
//   - PS 7+ 分支：用 -Encoding ([Text.Encoding]::GetEncoding(936)) 显式 GBK，纯原生，
//     因为 [Text.Encoding]::Default 在 PS 7 下恒为 UTF-8 会乱码；
//   - PS 5.1 中文（ACP=936）：-Encoding Default 原生零开销；
//   - PS 5.1 非中文（ACP≠936）：逐行 GetEncoding('GBK') 转码；
//   - 系统 ACP 用 GetEncoding(0) 获取（两个版本都返回真实代码页，不能用 Default）；
//   - follow 模式必须保留 -Wait/-Tail 原生尾部定位，绝不能全量 ReadLines。
func TestWindowsEncodingStages(t *testing.T) {
	const path = `C:\logs\app.log`

	// UTF-8 follow：原生 -Encoding UTF8，不引入 GBK 分流
	utf8Follow := BuildView("windows", "follow", path, "utf-8", 100, FilterCfg{}).Script
	if !strings.Contains(utf8Follow, "-Encoding UTF8") {
		t.Errorf("UTF-8 follow 应使用 -Encoding UTF8:\n%s", utf8Follow)
	}
	if strings.Contains(utf8Follow, "PSVersionTable.PSVersion.Major") || strings.Contains(utf8Follow, "GetEncoding(936)") {
		t.Errorf("UTF-8 follow 不应走 GBK 分流:\n%s", utf8Follow)
	}

	// GBK follow：PS 版本分流 + 原生 -Wait/-Tail 尾部定位
	for _, enc := range []string{"gbk", "GBK", " gbk ", "gb2312"} {
		gbkFollow := BuildView("windows", "follow", path, enc, 100, FilterCfg{}).Script
		// 必须有 PS 7+ 分支（显式 GBK 编码对象）
		if !strings.Contains(gbkFollow, "PSVersionTable.PSVersion.Major -ge 7") {
			t.Errorf("GBK follow (%q) 必须按 PS 版本分流:\n%s", enc, gbkFollow)
		}
		if !strings.Contains(gbkFollow, "GetEncoding(936)") {
			t.Errorf("GBK follow (%q) PS7 分支必须用 GetEncoding(936) 显式 GBK:\n%s", enc, gbkFollow)
		}
		// PS 5.1 分支必须用 GetEncoding(0)（不是 Default）+ ACP=936 快路径
		if !strings.Contains(gbkFollow, "GetEncoding(0).CodePage -eq 936") {
			t.Errorf("GBK follow (%q) PS5.1 分支必须用 GetEncoding(0) 检测系统 ACP:\n%s", enc, gbkFollow)
		}
		if !strings.Contains(gbkFollow, "-Wait") {
			t.Errorf("GBK follow (%q) 必须带 -Wait 实时跟踪:\n%s", enc, gbkFollow)
		}
		if !strings.Contains(gbkFollow, "GetEncoding('GBK')") {
			t.Errorf("GBK follow (%q) 非 936 分支应逐行用 GetEncoding('GBK') 转码:\n%s", enc, gbkFollow)
		}
		// 绝不能出现裸露的 -Encoding Default（PS 7 下会乱码）
		if strings.Contains(gbkFollow, "-Encoding Default -Wait") || strings.Contains(gbkFollow, "-Encoding Default -Tail") {
			t.Errorf("GBK follow (%q) 禁止直接用 -Encoding Default（PS7 下恒为 UTF-8 乱码）:\n%s", enc, gbkFollow)
		}
	}

	// GBK follow limit>0：原生 -Wait -Tail N，不应有 ReadLines 全量枚举
	gbkTail := BuildView("windows", "follow", path, "gbk", 50, FilterCfg{}).Script
	if !strings.Contains(gbkTail, "-Tail 50") {
		t.Errorf("GBK follow limit=50 应使用原生 -Tail 50:\n%s", gbkTail)
	}
	if strings.Contains(gbkTail, "[IO.File]::ReadLines") || strings.Contains(gbkTail, "Select-Object -Last") {
		t.Errorf("GBK follow 不应全量枚举文件（ReadLines/Select -Last 性能退化）:\n%s", gbkTail)
	}

	// GBK follow limit=0：-Wait 不带 -Tail，仍有版本分流
	gbkNoTail := BuildView("windows", "follow", path, "gbk", 0, FilterCfg{}).Script
	if strings.Contains(gbkNoTail, "-Tail ") {
		t.Errorf("GBK follow limit=0 不应带 -Tail:\n%s", gbkNoTail)
	}
	if !strings.Contains(gbkNoTail, "PSVersionTable.PSVersion.Major -ge 7") {
		t.Errorf("GBK follow limit=0 仍需 PS 版本分流:\n%s", gbkNoTail)
	}

	// GBK static limit>0：原生 -Tail + PS 版本分流（非全量 ReadLines）
	gbkStaticTail := BuildView("windows", "static", path, "gbk", 100, FilterCfg{}).Script
	if !strings.Contains(gbkStaticTail, "-Tail 100") {
		t.Errorf("GBK static limit=100 应用原生 -Tail 100:\n%s", gbkStaticTail)
	}
	if !strings.Contains(gbkStaticTail, "PSVersionTable.PSVersion.Major -ge 7") {
		t.Errorf("GBK static limit=100 应 PS 版本分流:\n%s", gbkStaticTail)
	}
	if strings.Contains(gbkStaticTail, "[IO.File]::ReadLines") {
		t.Errorf("GBK static limit>0 不应全量 ReadLines:\n%s", gbkStaticTail)
	}

	// GBK static no-limit：显式 ReadLines(GBK)，全量枚举（无尾部定位需求，跨版本/区域正确）
	gbkStatic := BuildView("windows", "static", path, "gbk", 0, FilterCfg{}).Script
	if !strings.Contains(gbkStatic, "[IO.File]::ReadLines(") || !strings.Contains(gbkStatic, "GetEncoding('GBK')") {
		t.Errorf("GBK static no-limit 应用 ReadLines + GetEncoding('GBK'):\n%s", gbkStatic)
	}

	// UTF-8 static no-limit 用 ReadLines UTF8（比 Get-Content 快）
	utf8StaticNoLimit := BuildView("windows", "static", path, "utf-8", 0, FilterCfg{}).Script
	if !strings.Contains(utf8StaticNoLimit, "[IO.File]::ReadLines") {
		t.Errorf("UTF-8 static no-limit 应使用 ReadLines:\n%s", utf8StaticNoLimit)
	}

	// UTF-8 static limit>0 用 Get-Content -Tail
	utf8StaticTail := BuildView("windows", "static", path, "utf-8", 100, FilterCfg{}).Script
	if !strings.Contains(utf8StaticTail, "-Tail 100") {
		t.Errorf("UTF-8 static limit=100 应使用 -Tail 100:\n%s", utf8StaticTail)
	}
}

func TestIsGBK(t *testing.T) {
	cases := map[string]bool{
		"gbk": true, "GBK": true, " gbk ": true, "gb2312": true, "GB2312": true,
		"utf-8": false, "UTF8": false, "": false, "gbk ": true,
	}
	for in, want := range cases {
		if got := IsGBK(in); got != want {
			t.Errorf("IsGBK(%q) = %v, want %v", in, got, want)
		}
	}
}
