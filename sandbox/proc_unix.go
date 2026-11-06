//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// prepareProcess 让命令**自成进程组**：超时终止时整组击杀，命令自己
// fork 出的孤儿进程一起死（否则它们会继续跑）。
func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree 杀掉命令及其后代（整组）。
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// 负 pid = 进程组（Setpgid 建的组）。
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
