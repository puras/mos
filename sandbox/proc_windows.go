//go:build windows

package sandbox

import (
	"os/exec"
	"strconv"
	"syscall"
)

// Windows 的进程参数。
//
// 没有 Unix 的进程组概念（Setpgid 没有对应物），整树终止靠 taskkill /T。
const (
	// createNewProcessGroup 让子进程自成进程组（Ctrl+C 不波及父进程）。
	createNewProcessGroup = 0x00000200
	// createNoWindow 不弹控制台窗口——服务端场景不该给用户闪一个黑框。
	createNoWindow = 0x08000000
)

// prepareProcess 配好子进程的 Windows 进程属性。
func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | createNoWindow,
	}
}

// killProcessTree 杀掉命令及其后代。
//
// 用 Windows 自带的 taskkill /T（按进程树终止）——它处理的是**整棵树**
// （命令 fork 出来的子进程一起死），跟 unix 的进程组击杀语义对齐。
// taskkill 不在 PATH 时退回 Process.Kill（只杀自己，孤儿可能留下——
// 诚实降级：Windows 上没有 Unix 那种进程组原语）。
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if killer, err := exec.LookPath("taskkill"); err == nil {
		_ = exec.Command(killer, "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return
	}
	_ = cmd.Process.Kill()
}
