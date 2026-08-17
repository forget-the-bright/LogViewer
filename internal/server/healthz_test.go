package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"logviewer/internal/appconfig"
	"logviewer/internal/config"
	"logviewer/internal/host"
	"logviewer/internal/metrics"
)

// newTestServerWithLocal 构造一个只含本机 host 的 Server 与路由，用于可观测性端点测试。
func newTestServerWithLocal(t *testing.T) *Server {
	t.Helper()
	local, err := host.NewLocalHost("local", []string{t.TempDir()}, nil, config.NewConfigStore(), nil)
	if err != nil {
		t.Fatalf("NewLocalHost: %v", err)
	}
	hm, err := host.NewManager(local)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return New(Options{
		Hosts: hm,
		Auth:  appconfig.AuthConfig{},
	})
}

func TestHealthzOK(t *testing.T) {
	srv := newTestServerWithLocal(t)
	r := srv.Router()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		Hosts  []struct {
			Name      string `json:"name"`
			Available bool   `json:"available"`
			Online    bool   `json:"online"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, w.Body.String())
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if len(body.Hosts) != 1 || body.Hosts[0].Name != "local" {
		t.Fatalf("hosts = %+v, want single local host", body.Hosts)
	}
	if !body.Hosts[0].Available {
		t.Errorf("local host should be available")
	}
}

func TestMetricsEndpointExposesRegistry(t *testing.T) {
	srv := newTestServerWithLocal(t)
	r := srv.Router()

	// /metrics 用的是包级 Default 单例；给每个 CounterVec 写一个标签值，
	// 否则零值的 CounterVec 族不会出现在输出里（Gauge 族恒在）。
	metrics.WSInc("local")
	metrics.WSDec("local")
	metrics.IncSSHReconnect("local")
	metrics.IncExportBytes("origin", 1)
	metrics.IncLogBytes(1)
	metrics.SetProcesses(0)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// 端点应至少暴露我们注册的指标名（即便数值为 0，指标族也应出现）。
	for _, want := range []string{
		"logviewer_ws_connections",
		"logviewer_log_processes",
		"logviewer_ssh_reconnects_total",
		"logviewer_export_bytes_total",
		"logviewer_log_bytes_sent_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics output missing %q", want)
		}
	}
}

func TestObservabilityEndpointsNoAuth(t *testing.T) {
	// 即便启用了登录认证，/healthz 与 /metrics 也应免鉴权可抓取。
	local, err := host.NewLocalHost("local", []string{t.TempDir()}, nil, config.NewConfigStore(), nil)
	if err != nil {
		t.Fatalf("NewLocalHost: %v", err)
	}
	hm, _ := host.NewManager(local)
	srv := New(Options{
		Hosts: hm,
		Auth: appconfig.AuthConfig{
			Enabled:  true,
			Username: "admin",
			Password: "secret",
		},
	})
	r := srv.Router()
	for _, path := range []string{"/healthz", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Errorf("%s should be exempt from auth, got %d", path, w.Code)
		}
	}
}
