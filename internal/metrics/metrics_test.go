package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestMetrics 构造一组注册到独立 registry 的指标，避免污染全局默认注册器。
func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	return New(prometheus.NewRegistry())
}

func TestWSConnGauge(t *testing.T) {
	m := newTestMetrics(t)
	m.WSInc("local")
	m.WSInc("local")
	m.WSInc("prod-01")
	if got := testutil.ToFloat64(m.wsConn.WithLabelValues("local")); got != 2 {
		t.Errorf("local ws conns = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.wsConn.WithLabelValues("prod-01")); got != 1 {
		t.Errorf("prod-01 ws conns = %v, want 1", got)
	}
	m.WSDec("local")
	if got := testutil.ToFloat64(m.wsConn.WithLabelValues("local")); got != 1 {
		t.Errorf("after dec local ws conns = %v, want 1", got)
	}
}

func TestLogProcsGauge(t *testing.T) {
	m := newTestMetrics(t)
	m.SetProcesses(3)
	if got := testutil.ToFloat64(m.logProcs); got != 3 {
		t.Errorf("log_procs = %v, want 3", got)
	}
	m.SetProcesses(0)
	if got := testutil.ToFloat64(m.logProcs); got != 0 {
		t.Errorf("log_procs after reset = %v, want 0", got)
	}
}

func TestSSHReconnectCounter(t *testing.T) {
	m := newTestMetrics(t)
	m.IncSSHReconnect("prod-01")
	m.IncSSHReconnect("prod-01")
	m.IncSSHReconnect("prod-02")
	if got := testutil.ToFloat64(m.sshReconn.WithLabelValues("prod-01")); got != 2 {
		t.Errorf("prod-01 reconnects = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.sshReconn.WithLabelValues("prod-02")); got != 1 {
		t.Errorf("prod-02 reconnects = %v, want 1", got)
	}
}

func TestExportAndLogBytesCounters(t *testing.T) {
	m := newTestMetrics(t)
	m.IncExportBytes("origin", 100)
	m.IncExportBytes("filter", 50)
	m.IncExportBytes("filter", 25)
	// 负数/0 不应计入
	m.IncExportBytes("origin", -1)
	if got := testutil.ToFloat64(m.exportBytes.WithLabelValues("origin")); got != 100 {
		t.Errorf("origin export bytes = %v, want 100", got)
	}
	if got := testutil.ToFloat64(m.exportBytes.WithLabelValues("filter")); got != 75 {
		t.Errorf("filter export bytes = %v, want 75", got)
	}

	m.IncLogBytes(1024)
	m.IncLogBytes(0)
	if got := testutil.ToFloat64(m.logBytes); got != 1024 {
		t.Errorf("log bytes = %v, want 1024", got)
	}
}

func TestHandlerServesMetrics(t *testing.T) {
	m := newTestMetrics(t)
	m.WSInc("local")
	m.IncLogBytes(42)
	// CounterVec 至少写一个标签值，指标族才会出现在输出里。
	m.IncSSHReconnect("local")
	m.IncExportBytes("origin", 1)
	m.SetProcesses(0)
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	for _, want := range []string{
		"logviewer_ws_connections",
		"logviewer_log_processes",
		"logviewer_ssh_reconnects_total",
		"logviewer_export_bytes_total",
		"logviewer_log_bytes_sent_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}
