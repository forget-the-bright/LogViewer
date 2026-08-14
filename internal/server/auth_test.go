package server

import (
	"testing"
	"time"

	"logviewer/internal/appconfig"
)

func testAuth() *authService {
	return newAuthService(appconfig.AuthConfig{
		Enabled:           true,
		Username:          "admin",
		Password:          "secret",
		SessionTTLMinutes: 120,
	})
}

func TestAuthDisabled(t *testing.T) {
	a := newAuthService(appconfig.AuthConfig{})
	if a.Enabled() {
		t.Fatal("空用户名应视为未启用认证")
	}
	if a.validate("any") {
		t.Fatal("未启用认证时 validate 不应被调用为 true")
	}
}

func TestLoginSuccessAndValidate(t *testing.T) {
	a := testAuth()
	token, ok := a.Login("admin", "secret", "1.2.3.4")
	if !ok || token == "" {
		t.Fatalf("正确凭据应登录成功, ok=%v", ok)
	}
	if !a.validate(token) {
		t.Fatal("登录后 token 应有效")
	}
}

func TestLoginWrongPassword(t *testing.T) {
	old := loginDelay
	loginDelay = 0
	defer func() { loginDelay = old }()

	a := testAuth()
	if _, ok := a.Login("admin", "wrong", "1.2.3.4"); ok {
		t.Fatal("错误密码不应登录成功")
	}
	if _, ok := a.Login("root", "secret", "1.2.3.4"); ok {
		t.Fatal("错误用户名不应登录成功")
	}
}

func TestLogout(t *testing.T) {
	a := testAuth()
	token, _ := a.Login("admin", "secret", "ip")
	a.logout(token)
	if a.validate(token) {
		t.Fatal("登出后 token 应失效")
	}
}

func TestValidateUnknownToken(t *testing.T) {
	a := testAuth()
	if a.validate("nonexistent") {
		t.Fatal("不存在的 token 应无效")
	}
}

func TestRateLimit(t *testing.T) {
	old := loginDelay
	loginDelay = 0
	defer func() { loginDelay = old }()

	a := testAuth()
	ip := "9.9.9.9"
	// 连续 maxFailAttempts 次失败
	for i := 0; i < maxFailAttempts; i++ {
		if _, ok := a.Login("admin", "bad", ip); ok {
			t.Fatalf("第 %d 次应失败", i)
		}
	}
	// 之后即使密码正确也应被限流拒绝
	if _, ok := a.Login("admin", "secret", ip); ok {
		t.Fatal("达到失败上限后应被限流，即使密码正确")
	}
	// 其它 IP 不受影响
	if _, ok := a.Login("admin", "secret", "8.8.8.8"); !ok {
		t.Fatal("不同 IP 不应被限流")
	}
}

func TestSlidingExpiry(t *testing.T) {
	old := loginDelay
	loginDelay = 0
	defer func() { loginDelay = old }()

	a := testAuth()
	a.ttl = 100 * time.Millisecond
	token, ok := a.Login("admin", "secret", "ip")
	if !ok {
		t.Fatal("登录失败")
	}
	time.Sleep(40 * time.Millisecond)
	if !a.validate(token) {
		t.Fatal("未过期 token 应有效")
	}
	time.Sleep(40 * time.Millisecond)
	// 距上次 validate 40ms < 100ms，且上次 validate 已续期，应仍有效
	if !a.validate(token) {
		t.Fatal("滑动续期后 token 应仍有效")
	}
	time.Sleep(120 * time.Millisecond)
	if a.validate(token) {
		t.Fatal("超过 TTL 未活动应过期")
	}
}

func TestSameOriginHost(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://127.0.0.1:8080", "127.0.0.1:8080", true},
		{"http://localhost:8080", "127.0.0.1:8080", false},
		{"https://evil.com", "127.0.0.1:8080", false},
		{"http://127.0.0.1:8080/path", "127.0.0.1:8080", true},
		{"not-a-url", "127.0.0.1:8080", false},
	}
	for _, c := range cases {
		if got := sameOriginHost(c.origin, c.host); got != c.want {
			t.Errorf("sameOriginHost(%q,%q)=%v want %v", c.origin, c.host, got, c.want)
		}
	}
}
