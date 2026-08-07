package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"hegq.net/mos/pkg/tool"
)

const (
	shellTimeout  = 10 * time.Second
	shellMaxBytes = 64 << 10 // 输出上限 64KB
)

// Shell 执行本地 shell 命令（安全策略由上层 Harness 控制）。
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

	// 配置了执行沙箱（M1.6）时优先在沙箱内执行：工作目录 jail + 资源限制 + 进程组超时。
	if sb := tool.SandboxFrom(ctx); sb != nil {
		out, runErr := sb.Run(ctx, p.Command)
		res := map[string]any{"output": out}
		if runErr != nil {
			res["error"] = runErr.Error()
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				res["exit_code"] = ee.ExitCode()
			}
		}
		return res, nil
	}

	// 未配置沙箱：回退到默认执行方式（本地 /bin/sh + 超时 + 输出截断）。
	ctx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", p.Command)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	output := out.String()
	truncated := false
	if len(output) > shellMaxBytes {
		output = output[:shellMaxBytes]
		truncated = true
	}

	res := map[string]any{
		"output":    output,
		"exit_code": cmd.ProcessState.ExitCode(),
		"duration":  dur.String(),
	}
	if runErr != nil {
		res["error"] = runErr.Error()
	}
	if truncated {
		res["truncated"] = true
	}
	return res, nil
}

var _ tool.Tool = Shell{}
