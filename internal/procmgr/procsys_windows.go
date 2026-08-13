//go:build windows

package procmgr

import (
	"os/exec"
	"strconv"
)

// applyProcGroup Windows 下 exec 本身不提供进程组语义，留空；
// 进程树的终止由 killGroupByPid 用 taskkill /T 完成。
func applyProcGroup(cmd *exec.Cmd) {}

// killGroupByPid 用 taskkill /T /F 终止整个进程树。
// 虽然本工具的过滤管道通常在单个 powershell 进程内运行，但 powershell
// 可能派生子宿主（如某些原生命令、编码转换），/T 能确保连同子孙进程
// 一并终止，杜绝僵尸 powershell / conhost 残留。
func killGroupByPid(pid int) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
