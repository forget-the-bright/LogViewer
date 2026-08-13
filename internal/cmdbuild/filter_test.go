package cmdbuild

import (
	"regexp"
	"testing"

	"logviewer/internal/config"
)

func TestAssemblePattern(t *testing.T) {
	// 级别 + 内容 用 .* AND 连接
	got := AssemblePattern(config.FilterRule{
		Levels:  []string{"ERROR", "WARN"},
		Content: "连接失败",
	}, false)
	want := `(ERROR|WARN).*连接失败`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// 仅级别
	if g := AssemblePattern(config.FilterRule{Levels: []string{"ERROR"}}, false); g != "(ERROR)" {
		t.Errorf("level only: %q", g)
	}
	// 仅内容（字面量转义）
	if g := AssemblePattern(config.FilterRule{Content: "a.b"}, false); g != `a\.b` {
		t.Errorf("literal content: %q", g)
	}
	// 正则内容原样
	if g := AssemblePattern(config.FilterRule{Content: `a.b+`}, true); g != `a.b+` {
		t.Errorf("regex content: %q", g)
	}
	// 自定义正则仅在"正则"勾选时生效
	if g := AssemblePattern(config.FilterRule{Levels: []string{"ERROR"}, Content: "x", CustomRegex: "^CUSTOM"}, true); g != "^CUSTOM" {
		t.Errorf("custom priority: %q", g)
	}
	// 普通文本模式忽略自定义正则，按字面量内容匹配
	if g := AssemblePattern(config.FilterRule{Levels: []string{"ERROR"}, Content: "x", CustomRegex: "^CUSTOM"}, false); g != "(ERROR).*x" {
		t.Errorf("plain mode ignores custom: %q", g)
	}
	// 空规则
	if g := AssemblePattern(config.FilterRule{}, false); g != "" {
		t.Errorf("empty: %q", g)
	}
}

func TestTimeBounds(t *testing.T) {
	cases := []struct {
		name        string
		rule        config.FilterRule
		wantStart   string
		wantEnd     string
		wantEnabled bool
	}{
		{"empty", config.FilterRule{}, "", "", false},
		{"day", config.FilterRule{TimeStart: "2024-01-01", TimeEnd: "2024-01-01", TimePrecision: "day"},
			"2024-01-01 00:00:00", "2024-01-01 23:59:59", true},
		{"hour", config.FilterRule{TimeStart: "2024-01-01 07", TimeEnd: "2024-01-01 08", TimePrecision: "hour"},
			"2024-01-01 07:00:00", "2024-01-01 08:59:59", true},
		{"minute", config.FilterRule{TimeStart: "2024-01-01 07:01", TimeEnd: "2024-01-01 07:05", TimePrecision: "minute"},
			"2024-01-01 07:01:00", "2024-01-01 07:05:59", true},
		{"second", config.FilterRule{TimeStart: "2024-01-01 07:01:14", TimeEnd: "2024-01-01 07:02:30", TimePrecision: "second"},
			"2024-01-01 07:01:14", "2024-01-01 07:02:30", true},
		{"default second", config.FilterRule{TimeStart: "2024-01-01 07:01:14", TimeEnd: "2024-01-01 07:02:30"},
			"2024-01-01 07:01:14", "2024-01-01 07:02:30", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, e, ok := TimeBounds(c.rule)
			if ok != c.wantEnabled || s != c.wantStart || e != c.wantEnd {
				t.Errorf("got (%q,%q,%v) want (%q,%q,%v)", s, e, ok, c.wantStart, c.wantEnd, c.wantEnabled)
			}
		})
	}
}

// 验证时间戳正则能正确抓取日志行首的秒级时间戳
func TestTimeTokenPattern(t *testing.T) {
	re := regexp.MustCompile(timeTokenPattern)
	line := "2026-08-13 07:01:14.909 [http-nio] ERROR o.x - boom"
	m := re.FindString(line)
	if m != "2026-08-13 07:01:14" {
		t.Errorf("token = %q", m)
	}
	// 字典序比较 == 时间序，且秒级闭区间端点可直接比较
	if m < "2026-08-13 07:01:00" || m > "2026-08-13 07:01:59" {
		t.Error("lexicographic range check failed")
	}
}
