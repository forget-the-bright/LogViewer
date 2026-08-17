package server

import (
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"logviewer/internal/appconfig"
)

const (
	sessionCookie   = "lv_sess"
	failWindow      = time.Minute
	maxFailAttempts = 5
)

// loginDelay 失败后的统一延迟，减缓爆破。测试可覆盖以加速。
var loginDelay = time.Second

// authService 管理登录会话与失败限流。
//
// 会话只存内存（重启即失效，符合单用户工具的威胁模型）：token -> 过期时刻。
// 密码校验通过闭包委托给 appconfig.AuthConfig.ValidatePassword（支持 bcrypt 与明文常量时间比较），
// server 不直接持有密码哈希。
type authService struct {
	enabled  bool
	username string
	ttl      time.Duration
	check    func(plain string) bool

	mu       sync.Mutex
	sessions map[string]time.Time
	fails    map[string][]time.Time // ip -> 窗口内失败时间戳
}

func newAuthService(cfg appconfig.AuthConfig) *authService {
	a := &authService{
		enabled:  cfg.Enabled && cfg.Username != "",
		username: cfg.Username,
		ttl:      time.Duration(cfg.SessionTTLMinutes) * time.Minute,
		check:    cfg.ValidatePassword,
		sessions: map[string]time.Time{},
		fails:    map[string][]time.Time{},
	}
	if a.ttl <= 0 {
		a.ttl = 720 * time.Minute
	}
	return a
}

func (a *authService) Enabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enabled
}

func (a *authService) Username() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.username
}

// newToken 生成 32 字节随机 token（base64url 无填充）。
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// tooManyFailures 判断某 IP 是否在窗口内超限，并裁剪旧记录。调用方持锁。
// 当该 IP 的所有失败记录都已过期时，从 map 删除，避免 map 随爆破 IP 数量无限增长。
func (a *authService) tooManyFailures(ip string, now time.Time) bool {
	old := a.fails[ip]
	cutoff := now.Add(-failWindow)
	kept := old[:0]
	for _, t := range old {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(a.fails, ip)
	} else {
		a.fails[ip] = kept
	}
	return len(kept) >= maxFailAttempts
}

// Login 校验凭据，成功则创建会话并返回 token。
// 失败（含限流）统一等待 loginDelay，避免用户名枚举/爆破时序差异。
func (a *authService) Login(user, pass, ip string) (string, bool) {
	time.Sleep(loginDelay)

	a.mu.Lock()
	if a.tooManyFailures(ip, time.Now()) {
		a.mu.Unlock()
		return "", false
	}
	// 在锁内快照用户名/校验函数/TTL，避免与 UpdateAuth 的热更新产生数据竞争。
	expectedUser := a.username
	check := a.check
	ttl := a.ttl
	a.mu.Unlock()

	if check == nil || user != expectedUser || !check(pass) {
		a.mu.Lock()
		a.fails[ip] = append(a.fails[ip], time.Now())
		a.mu.Unlock()
		return "", false
	}

	token, err := newToken()
	if err != nil {
		return "", false
	}
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(ttl)
	// 登录成功清空该 IP 的失败计数
	delete(a.fails, ip)
	a.mu.Unlock()
	return token, true
}

// TTL 返回会话有效期（并发安全）。
func (a *authService) TTL() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ttl
}

// validate 校验 token 并滑动续期；过期/不存在返回 false。
func (a *authService) validate(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, token)
		return false
	}
	a.sessions[token] = time.Now().Add(a.ttl)
	return true
}

func (a *authService) logout(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

// clientIP 取真实客户端 IP（支持反代 X-Forwarded-For）。
func clientIP(c *gin.Context) string {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return c.Request.RemoteAddr
	}
	return host
}

// setSessionCookie 写入会话 cookie。tls 为 true 时加 Secure。
func setSessionCookie(c *gin.Context, token string, ttl time.Duration, tls bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   tls,
	})
}

func clearSessionCookie(c *gin.Context, tls bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   tls,
	})
}

func isTLS(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	return strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}

// authRequired 中间件：未启用认证直接放行；否则要求有效会话 cookie。
func (s *Server) authRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.auth.Enabled() {
			c.Next()
			return
		}
		if ck, err := c.Cookie(sessionCookie); err == nil && s.auth.validate(ck) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录或会话已过期"})
	}
}

// ---- handlers ----

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	token, ok := s.auth.Login(req.Username, req.Password, clientIP(c))
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误（连续失败多次将被暂时限流）"})
		return
	}
	setSessionCookie(c, token, s.auth.TTL(), isTLS(c))
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": s.auth.Username()})
}

func (s *Server) handleLogout(c *gin.Context) {
	ck, err := c.Cookie(sessionCookie)
	if err == nil {
		s.auth.logout(ck)
	}
	clearSessionCookie(c, isTLS(c))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleAuthStatus 返回是否启用认证及当前用户名，供前端决定显示登录遮罩/登出按钮。
// 不鉴权：未登录时前端也需要知道是否启用认证。
func (s *Server) handleAuthStatus(c *gin.Context) {
	resp := gin.H{"enabled": s.auth.Enabled()}
	if s.auth.Enabled() {
		resp["username"] = s.auth.Username()
		authed := false
		if ck, err := c.Cookie(sessionCookie); err == nil {
			authed = s.auth.validate(ck)
		}
		resp["authed"] = authed
	}
	c.JSON(http.StatusOK, resp)
}
