//go:build windows

package procmgr

import (
	"os/exec"
	"syscall"
	"testing"
)

// createNoWindow 即 Windows CREATE_NO_WINDOW (0x08000000)，是阻止子进程
// （PowerShell/conhost）在无控制台的 GUI 父进程下闪黑窗的关键创建标志。
// 这里以字面量重复断言，避免常量本身被误改而测试仍通过。
const wantCreateNoWindow = 0x08000000

// TestHideWindowSetsCreateNoWindow 验证 HideWindow 为命令设置 CREATE_NO_WINDOW，
// 并在 SysProcAttr 已存在时以按位或叠加、不破坏已有标志。
func TestHideWindowSetsCreateNoWindow(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	HideWindow(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("HideWindow 未设置 SysProcAttr")
	}
	if cmd.SysProcAttr.CreationFlags&wantCreateNoWindow == 0 {
		t.Fatalf("CreationFlags 未包含 CREATE_NO_WINDOW，got %#x", cmd.SysProcAttr.CreationFlags)
	}

	// 已有其他创建标志时应叠加而非覆盖。
	cmd2 := exec.Command("cmd.exe", "/c", "exit")
	cmd2.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x1} // CREATE_SUSPENDED 占位
	HideWindow(cmd2)
	if cmd2.SysProcAttr.CreationFlags&wantCreateNoWindow == 0 {
		t.Fatalf("叠加后丢失 CREATE_NO_WINDOW，got %#x", cmd2.SysProcAttr.CreationFlags)
	}
	if cmd2.SysProcAttr.CreationFlags&0x1 == 0 {
		t.Fatalf("叠加时覆盖了已有标志，got %#x", cmd2.SysProcAttr.CreationFlags)
	}

	// 幂等：重复调用不改变结果。
	HideWindow(cmd2)
	if cmd2.SysProcAttr.CreationFlags != 0x1|wantCreateNoWindow {
		t.Fatalf("重复调用非幂等，got %#x", cmd2.SysProcAttr.CreationFlags)
	}

	// nil 安全。
	HideWindow(nil)
}

// TestHideWindowUsedByLocalProcessStart 验证走 LocalProcess 的长进程路径在
// Start 前也带上 CREATE_NO_WINDOW（localProc.Start 内部调用 applyProcGroup，
// Windows 上即 HideWindow）。Start 一个立即退出的命令验证真实启动链路。
func TestHideWindowUsedByLocalProcessStart(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	p := LocalProcess(cmd)
	if err := p.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&wantCreateNoWindow == 0 {
		t.Fatalf("LocalProcess 启动的命令缺少 CREATE_NO_WINDOW，flags=%v", cmd.SysProcAttr)
	}
	_ = p.Wait()
}
