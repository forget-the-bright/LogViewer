//go:build !windows

package procmgr

import (
	"os/exec"
	"syscall"
)

// HideWindow 在非 Windows 平台是空操作：Unix 子进程没有控制台窗口概念。
func HideWindow(cmd *exec.Cmd) {}

// applyProcGroup 让子进程以独立进程组运行，便于连同整条管道一并终止
func applyProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroupByPid 向整个进程组发送 SIGKILL（覆盖 sh 及其子进程 tail/grep/iconv）
func killGroupByPid(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}