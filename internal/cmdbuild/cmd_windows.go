//go:build windows

package cmdbuild

import (
	"os/exec"

	"logviewer/internal/procmgr"
)

// applyPlatformDefaults 在 Windows 上为本机命令设置 CREATE_NO_WINDOW。
// 这样任何从 BuildCmd 产出的命令（无论后续走 procmgr 长进程路径，还是直接
// CombinedOutput 的短命命令）天生不弹控制台，杜绝"忘记包装就闪黑窗"的回归。
func applyPlatformDefaults(cmd *exec.Cmd) {
	procmgr.HideWindow(cmd)
}
