package server

import (
	"strings"
	"testing"
)

func TestContentDisposition(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		wantSub  []string // 必须出现的子串
	}{
		{
			name:     "ascii",
			filename: "app_20260816.log",
			wantSub:  []string{`attachment; filename="app_20260816.log"`, "filename*=UTF-8''app_20260816.log"},
		},
		{
			name:     "chinese",
			filename: "应用日志.log",
			// ASCII 回退不能含裸非 ASCII；必须有 UTF-8 百分号编码的 filename*
			wantSub: []string{`filename="`, `.log"`, `filename*=UTF-8''`, "%E5%BA%94%E7%94%A8"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := contentDisposition(c.filename)
			for _, sub := range c.wantSub {
				if !strings.Contains(got, sub) {
					t.Errorf("contentDisposition(%q) = %q, want substring %q", c.filename, got, sub)
				}
			}
			// ASCII 回退文件名里不得出现裸非 ASCII 或引号/反斜杠
			i := strings.Index(got, `filename="`)
			if i < 0 {
				t.Fatalf("缺少 filename=: %q", got)
			}
			rest := got[i+len(`filename="`):]
			end := strings.Index(rest, `"`)
			if end < 0 {
				t.Fatalf("filename 未闭合: %q", got)
			}
			ascii := rest[:end]
			for _, r := range ascii {
				if r > 0x7e || r < 0x20 {
					t.Errorf("ASCII 回退含非法字符 %q in %q", r, ascii)
				}
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"one":          "one",
		"  two  \nbar": "two",
		"\n\nfirst":    "first",
		"grep: bad\nmore": "grep: bad",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
