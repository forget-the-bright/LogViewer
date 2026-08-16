package procmgr

import (
	"errors"
	"io"
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
