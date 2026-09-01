package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"

	"hegq.net/mos/sandbox"
	"hegq.net/mos/tool"
)

// Shell 执行本地 shell 命令（v2.24：经 sandbox.Runner 隔离，ctx 注入可换）。
// 未注入 Runner 时 sandbox.FromContext 返回 Default（进程组击杀兜底）。
type Shell struct{}

type shellParams struct {
	Command string `json:"command" mos:"desc=要执行的 shell 命令，如 ls -la"`
}

func (Shell) Name() string { return "shell" }
func (Shell) Description() string {
	return "在本地执行 shell 命令并返回输出，用于文件操作与系统信息查询"
}
func (Shell) Parameters() any { return &shellParams{} }
func (Shell) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[shellParams](args)
	if err != nil {
		return nil, err
	}
	if p.Command == "" {
		return nil, fmt.Errorf("命令不能为空")
	}

	// 统一走 sandbox.Runner seam：未注入时 FromContext 返回 Default（进程组击杀兜底，
	// 修复 v2.23 前默认路径只用 exec.CommandContext 不杀子进程的 bug）。
	out, runErr := sandbox.FromContext(ctx).Run(ctx, p.Command)
	res := map[string]any{"output": out, "exit_code": 0}
	if runErr != nil {
		res["error"] = runErr.Error()
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res["exit_code"] = ee.ExitCode()
		}
	}
	return res, nil
}

var _ tool.Tool = Shell{}