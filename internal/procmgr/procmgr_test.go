package procmgr

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeProc 模拟一个 StdoutPipe 已分配资源、但 Start 必然失败的进程
// （对应 SSH：StdoutPipe 已 NewSession，Start 失败后必须被 Kill 回收）。
type fakeProc struct {
	killCalled bool
}

func (p *fakeProc) StdoutPipe() (io.Reader, error) { return strings.NewReader(""), nil }
func (p *fakeProc) StderrPipe() (io.Reader, error) { return strings.NewReader(""), nil }
func (p *fakeProc) Start() error                    { return errors.New("start boom") }
func (p *fakeProc) Kill() error                     { p.killCalled = true; return nil }
func (p *fakeProc) Wait() error                     { return nil }

// TestStartFailureReleasesResources 验证 Start() 失败时会调用 Kill 回收
// StdoutPipe 已分配的资源（根治 SSH session/channel 泄漏）。
func TestStartFailureReleasesResources(t *testing.T) {
	m := NewManager()
	p := &fakeProc{}
	_, err := m.Start(p, nil, nil, nil)
	if err == nil {
		t.Fatal("期望 Start 返回错误，实际为 nil")
	}
	if !p.killCalled {
		t.Fatal("Start 失败后未调用 Kill，远程 session/channel 会泄漏")
	}

	// 等待足够时间确认没有泄漏的读取协程写入
	time.Sleep(100 * time.Millisecond)
}

// goodProc 正常进程，用于验证成功路径不会被误杀。
type goodProc struct {
	killCalled bool
}

func (p *goodProc) StdoutPipe() (io.Reader, error) { return strings.NewReader(""), nil }
func (p *goodProc) StderrPipe() (io.Reader, error) { return strings.NewReader(""), nil }
func (p *goodProc) Start() error                    { return nil }
func (p *goodProc) Kill() error                     { p.killCalled = true; return nil }
func (p *goodProc) Wait() error                     { return nil }

func TestStartSuccessNoImmediateKill(t *testing.T) {
	m := NewManager()
	p := &goodProc{}
	id, err := m.Start(p, nil, nil, nil)
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if p.killCalled {
		t.Fatal("成功路径不应在启动后立即 Kill")
	}
	m.Stop(id)
}

// emitProc 模拟一个产生大量 stdout 的进程，用于压测 readLoop 的读取+批量 flush 吞吐。
type emitProc struct {
	r io.Reader
}

func (p *emitProc) StdoutPipe() (io.Reader, error) { return p.r, nil }
func (p *emitProc) StderrPipe() (io.Reader, error) { return strings.NewReader(""), nil }
func (p *emitProc) Start() error                    { return nil }
func (p *emitProc) Kill() error                     { return nil }
func (p *emitProc) Wait() error                     { return nil }

// BenchmarkReadLoopThroughput 压测服务端"读取→按行攒批→flush"管道的吞吐。
// 这是 Go 侧唯一承担的数据搬运环节（日志内容由原生 tail/grep 产出），验证大批量
// 历史数据（20 万行量级）能被快速排空、不会因逐行同步 WebSocket 写而堆积。
func BenchmarkReadLoopThroughput(b *testing.B) {
	const lines = 200_000
	var sb strings.Builder
	for i := 0; i < lines; i++ {
		sb.WriteString("2026-08-17 12:00:00 INFO  benchmark line with some padding to mimic a real log entry number ")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteByte('\n')
	}
	payload := sb.String()
	b.SetBytes(int64(len(payload) / lines))
	b.ResetTimer()

	for n := 0; n < b.N; n++ {
		m := NewManager()
		got := 0
		done := make(chan struct{})
		p := &emitProc{r: strings.NewReader(payload)}
		id, err := m.Start(p, func(batch string) {
			got += strings.Count(batch, "\n")
		}, nil, func() { close(done) })
		if err != nil {
			b.Fatalf("Start 失败: %v", err)
		}
		<-done
		if got != lines {
			b.Fatalf("排空行数 = %d, want %d", got, lines)
		}
		m.Stop(id)
	}
}
