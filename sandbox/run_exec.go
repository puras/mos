package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// runExec 用 os/exec 跑一条命令：内存里截断输出、超时杀整树。
//
// 这是**进程级**执行路径（不套路径强制）。unix 上它承载「写全开」的情况，
// Windows 上承载同样的「写全开」情况；需要路径强制时走各自的后端。
//
// inv 里可以带约束后端要的额外内容（Linux Landlock 启动器的载荷管道）。
func (s *Local) runExec(ctx context.Context, inv invocation, dir string, timeout time.Duration) (Result, error) {
	cmd := exec.Command(inv.argv[0], inv.argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(s.environment(), inv.env...)
	cmd.ExtraFiles = inv.files
	// 进程组：子进程单独成组，超时终止时整组击杀，避免孤儿继续跑
	prepareProcess(cmd)

	out := s.newOutputBuffer()
	cmd.Stdout = out
	cmd.Stderr = out

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("sandbox: 启动命令失败: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		output := out.finish()
		res := Result{
			Output:     output,
			ExitCode:   exitCodeOf(err),
			Truncated:  out.truncated,
			OutputPath: out.finishSpill(),
			Duration:   time.Since(start),
		}
		// 约束后端的启动器失败 = **沙箱没装上**（工具错误），不是命令的
		// 结果（模型命令自己返回 125 是正常结果）。两者处置不同：前者要
		// 报给宿主「这台机器上路径强制不可用」。
		if msg := launcherFailure(output); msg != "" {
			return res, errors.New(msg)
		}
		return res, nil
	case <-ctx.Done():
		// 整组击杀：命令自己 fork 出的子进程也要死
		killProcessTree(cmd)
		<-done
		return Result{
			Output:     out.finish() + "\n[命令执行超时，已终止]",
			ExitCode:   -1,
			Truncated:  out.truncated,
			OutputPath: out.finishSpill(),
			Duration:   time.Since(start),
		}, fmt.Errorf("sandbox: 命令执行超时（%v）", timeout)
	}
}
