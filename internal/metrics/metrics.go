// Package metrics 集中暴露 LogViewer 自身的 Prometheus 指标。
//
// 设计原则：
//   - 业务层只调用本包的语义化函数（WSInc/IncSSHReconnect/...），不直接接触
//     prometheus API，便于在测试中替换实现；
//   - 所有指标通过 Handler() 暴露在 /metrics，供 Prometheus 抓取；
//   - 指标只含主机别名、导出种类等低敏维度，绝不包含日志内容、路径参数或密码。
package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// 指标命名空间前缀。
const ns = "logviewer"

// Metrics 持有本进程全部 Prometheus 指标。零值不可用，用 New 构造。
type Metrics struct {
	registry *prometheus.Registry

	wsConn      *prometheus.GaugeVec
	logProcs    prometheus.Gauge
	sshReconn   *prometheus.CounterVec
	exportBytes *prometheus.CounterVec
	logBytes    prometheus.Counter
}

var (
	defaultOnce sync.Once
	def         *Metrics
)

// Default 返回进程级单例。首次调用时完成注册；业务层可直接用包级函数。
func Default() *Metrics {
	defaultOnce.Do(func() { def = New(prometheus.DefaultRegisterer) })
	return def
}

// New 在给定 registerer（通常是 prometheus.DefaultRegisterer 或测试用 registry）
// 上注册并返回一组指标。
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		wsConn: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "ws_connections",
			Help:      "当前活跃的 WebSocket 连接数，按主机别名维度。",
		}, []string{"host"}),
		logProcs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "log_processes",
			Help:      "当前正在运行的日志读取/跟踪子进程数（本机 + 远程合计）。",
		}),
		sshReconn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "ssh_reconnects_total",
			Help:      "SSH 主机累计重连次数，按主机别名维度。",
		}, []string{"host"}),
		exportBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "export_bytes_total",
			Help:      "日志导出累计字节数，按导出种类（origin/filter）维度。",
		}, []string{"kind"}),
		logBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "log_bytes_sent_total",
			Help:      "通过 WebSocket 下发给前端的日志字节总数。",
		}),
	}
	// 同时注册到默认 registerer（/metrics 用）与自带 registry（测试用）。
	reg.MustRegister(m.wsConn, m.logProcs, m.sshReconn, m.exportBytes, m.logBytes)
	m.registry.MustRegister(m.wsConn, m.logProcs, m.sshReconn, m.exportBytes, m.logBytes)
	return m
}

// Handler 返回 /metrics 的 HTTP handler。
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// WSInc 在新建 WS 连接时调用。
func (m *Metrics) WSInc(host string) { m.wsConn.WithLabelValues(host).Inc() }

// WSDec 在 WS 连接关闭时调用。
func (m *Metrics) WSDec(host string) { m.wsConn.WithLabelValues(host).Dec() }

// SetProcesses 设置当前运行中的日志进程数。
func (m *Metrics) SetProcesses(n int) { m.logProcs.Set(float64(n)) }

// IncSSHReconnect 在某个 SSH 主机发生一次重连时调用。
func (m *Metrics) IncSSHReconnect(host string) { m.sshReconn.WithLabelValues(host).Inc() }

// IncExportBytes 累加导出字节数。kind 为 "origin" 或 "filter"。
func (m *Metrics) IncExportBytes(kind string, n int64) {
	if n > 0 {
		m.exportBytes.WithLabelValues(kind).Add(float64(n))
	}
}

// IncLogBytes 累加 WS 下发的日志字节数。
func (m *Metrics) IncLogBytes(n int) {
	if n > 0 {
		m.logBytes.Add(float64(n))
	}
}

// ---- 包级快捷函数，内部转发到 Default 单例 ----

// Handler 返回默认指标集的 /metrics handler。
func Handler() http.Handler { return Default().Handler() }

// WSInc 包级封装。
func WSInc(host string) { Default().WSInc(host) }

// WSDec 包级封装。
func WSDec(host string) { Default().WSDec(host) }

// SetProcesses 包级封装。
func SetProcesses(n int) { Default().SetProcesses(n) }

// IncSSHReconnect 包级封装。
func IncSSHReconnect(host string) { Default().IncSSHReconnect(host) }

// IncExportBytes 包级封装。
func IncExportBytes(kind string, n int64) { Default().IncExportBytes(kind, n) }

// IncLogBytes 包级封装。
func IncLogBytes(n int) { Default().IncLogBytes(n) }
