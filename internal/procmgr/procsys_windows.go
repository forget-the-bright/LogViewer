//go:build windows

package procmgr

import (
	"os/exec"
	"strconv"
	"syscall"
)

// createNoWindow 阻止子进程弹出控制台窗口。GUI 模式（父进程是 windowsgui
// 子系统、无控制台）下，PowerShell/conhost 会各自闪一个黑窗；web 模式下也能
// 避免多余的 conhost 窗口。与 PowerShell 的 -WindowStyle Hidden 叠加更彻底。
const createNoWindow = 0x08000000

// HideWindow 为 cmd 设置 CREATE_NO_WINDOW，使其不分配/不弹出控制台窗口。
// 所有本机启动子进程的路径（长进程经 procmgr、短命命令经 RunOneShot）都必须
// 调用它，否则在无控制台的 GUI 父进程下会闪黑窗。幂等，可重复调用。
func HideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// applyProcGroup 为子进程设置 CREATE_NO_WINDOW。
// exec 在 Windows 不提供进程组语义，进程树终止由 killGroupByPid 用 taskkill /T 完成。
func applyProcGroup(cmd *exec.Cmd) {
	HideWindow(cmd)
}

// killGroupByPid 用 taskkill /T /F 终止整个进程树。
// 虽然本工具的过滤管道通常在单个 powershell 进程内运行，但 powershell
// 可能派生子宿主（如某些原生命令、编码转换），/T 能确保连同子孙进程
// 一并终止，杜绝僵尸 powershell / conhost 残留。
func killGroupByPid(pid int) {
	taskkill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	applyProcGroup(taskkill)
	_ = taskkill.Run()
}
