package cmdbuild

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// runCheckLocal 在本机执行 BuildRegexCheck 产生的命令，返回退出码与合并输出。
func runCheckLocal(t *testing.T, platform, pattern string, caseSensitive bool) (int, string) {
	t.Helper()
	c := BuildRegexCheck(platform, pattern, caseSensitive)
	cmd := c.BuildCmd()
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("执行校验命令失败: %v (%s)", err, string(out))
	return -1, string(out)
}

// TestBuildRegexCheck_ValidInvalid 用本机原生引擎验证：
// 合法正则退出码 0；非法正则非 0 且有可读输出。
func TestBuildRegexCheck_ValidInvalid(t *testing.T) {
	platform := runtime.GOOS
	if platform != "linux" && platform != "darwin" && platform != "windows" {
		t.Skipf("平台 %s 不参与该校验测试", platform)
	}

	valid := []string{
		"ERROR",
		"(ERROR|WARN)",
		"^[0-9]{4}-",
		"foo.*bar",
	}
	for _, p := range valid {
		code, out := runCheckLocal(t, platform, p, false)
		if code != 0 {
			t.Errorf("合法正则 %q 被误判为非法（code=%d, out=%s）", p, code, out)
		}
	}

	// 未闭合分组 / 未闭合字符类：所有引擎都应报错。
	invalid := []string{
		"(ERROR|WARN",   // 未闭合括号
		"[0-9",          // 未闭合字符类
		"foo{5,3}",      // 重复区间颠倒
	}
	for _, p := range invalid {
		code, out := runCheckLocal(t, platform, p, false)
		if code == 0 {
			t.Errorf("非法正则 %q 被误判为合法", p)
			continue
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("非法正则 %q 报错但没有任何输出信息", p)
		}
	}
}
