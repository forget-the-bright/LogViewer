//go:build !windows

package cmdbuild

import "os/exec"

// applyPlatformDefaults 在非 Windows 平台为空操作（Unix 子进程无控制台窗口概念）。
func applyPlatformDefaults(cmd *exec.Cmd) {}
