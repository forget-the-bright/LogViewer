//go:build windows

package cmdbuild

import (
	"testing"
)

// TestBuildCmdSetsNoWindow 验证 BuildCmd 产出的本机命令在 Windows 上自带
// CREATE_NO_WINDOW——这是 GUI 模式（无控制台父进程）下不闪黑窗的根治保证。
// 覆盖长进程（LocalProcess）与短命命令（RunOneShot，直接 BuildCmd().CombinedOutput）
// 两条路径，因为两者都从 BuildCmd 取 *exec.Cmd。
func TestBuildCmdSetsNoWindow(t *testing.T) {
	const wantCreateNoWindow = 0x08000000

	for _, shell := range []string{"powershell", "sh"} {
		c := Command{Platform: "windows", Shell: shell, Script: "exit"}
		cmd := c.BuildCmd()
		if cmd.SysProcAttr == nil {
			t.Errorf("shell=%s: BuildCmd 未设置 SysProcAttr（缺少 CREATE_NO_WINDOW）", shell)
			continue
		}
		if cmd.SysProcAttr.CreationFlags&wantCreateNoWindow == 0 {
			t.Errorf("shell=%s: CreationFlags=%#x 未包含 CREATE_NO_WINDOW", shell, cmd.SysProcAttr.CreationFlags)
		}
	}
}
