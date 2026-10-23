package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/puras/mos/sandbox"
	"github.com/puras/mos/tool"
)

// Shell 执行一条 shell 命令（经 sandbox 隔离执行）。
//
// 三条纪律：
//   - **fail-closed**：没装配 sandbox.Runner 就不执行——不存在"没配沙箱就跑裸
//     命令"的默认路径（归档曾有该缺陷，用兜底 Runner 修）。
//   - **命令跑了但失败 ≠ 工具坏了**：非零退出码是**正常结果**（回执里带
//     exit_code，输出末尾附 [exit code: N] 标记），只有"命令根本没起来"
//     （启动失败 / 超时被杀 / 沙箱拒绝）才算错误（对齐 DSH：
//     packages/shell/tool-bash/src/render.ts——那里非零退出同样不是 isError，
//     isError 只留给 spawn 错误与中断）。这个区分很重要：把非零退出当失败，
//     模型就得自己写 hack 绕开（实测：它会拼 `; echo "exit=$?"` 让整条命令
//     以 0 退出，把失败塞进输出里）。
//   - **输出截断有逃生通道**：超限时全文落盘并回传路径，模型可以 read_file 读
//     全文（PI 的 full_output_path 对位，PRD 的"落盘逃生通道"）。
//
// 工作目录由宿主在 sandbox.Runner 上配置，**不由模型指定**——那等于给了一个
// 逃逸口（cd 到任意目录）。
type Shell struct {
	// Runner 是命令执行器（必填；nil = fail-closed 拒绝执行）。
	// 输出落盘由 Runner 自己管（sandbox.Local.SpillDir）——它才是看得见完整
	// 输出的那一层，工具只透传它给的路径。
	Runner sandbox.Runner
}

type shellParams struct {
	Command     string `json:"command" mos:"desc=要执行的 shell 命令"`
	Description string `json:"description,omitempty" mos:"desc=这条命令要做什么（人类可读，便于审计）"`
	Timeout     int    `json:"timeout,omitempty" mos:"desc=超时秒数；不传用宿主的缺省值，要求更久会被夹到宿主的上界"`
}

// NewShell 构造 shell 工具。
func NewShell(runner sandbox.Runner) *Shell {
	return &Shell{Runner: runner}
}

func (Shell) Name() string { return "shell" }

func (Shell) Description() string {
	return "执行一条 shell 命令（隔离执行：超时、输出上限、资源限制、环境裁剪）。" +
		"可用于验证改动（go build / go test / git diff）。" +
		"非零退出码是正常结果（回执里带 exit_code，输出末尾有 [exit code: N] 标记），" +
		"命令根本没跑起来（超时/被拒）才报错。输出过长会被截断，" +
		"截断时 output_path 指向全文文件。"
}

func (Shell) Parameters() any { return &shellParams{} }

// Execute 实现 tool.Tool.Execute。
func (s *Shell) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[shellParams](args)
	if err != nil {
		return nil, err
	}
	if p.Command == "" {
		return nil, fmt.Errorf("command 不能为空")
	}
	if s == nil || s.Runner == nil {
		return nil, &sandbox.NotConfiguredError{}
	}
	if p.Timeout < 0 {
		return nil, fmt.Errorf("timeout 必须为正数秒数")
	}

	// 模型没指定超时就不指定——由 Runner 用宿主的配置（Timeout 缺省、
	// MaxTimeout 上界）。工具不替宿主决定超时策略。
	timeout := time.Duration(p.Timeout) * time.Second
	res, err := s.Runner.Run(ctx, sandbox.Request{Command: p.Command, Timeout: timeout})
	if err != nil {
		// 无法执行（启动失败 / 超时）：把已有的输出也带回去，模型能看出跑到哪了。
		msg := fmt.Sprintf("命令执行失败: %v", err)
		if res.Output != "" {
			msg += "\n" + res.Output
		}
		return nil, fmt.Errorf("%s", msg)
	}

	out := map[string]any{
		"output":      res.Output,
		"exit_code":   res.ExitCode,
		"truncated":   res.Truncated,
		"duration_ms": res.Duration.Milliseconds(),
		"description": p.Description,
	}
	// 截断时 Runner 会把完整输出落盘，给模型一条逃生通道（它可以 read_file
	// 读全文——内存里那份已经被截断了）。
	if res.OutputPath != "" {
		out["output_path"] = res.OutputPath
	}
	if res.ExitCode != 0 {
		// **非零退出码是正常结果**，不是工具故障：命令跑了，只是没成功。
		// 回执带 exit_code，输出末尾再补一个 [exit code: N] 标记——对齐 DSH
		// （packages/shell/tool-bash/src/render.ts），那里非零退出同样不是
		// isError，isError 只留给 spawn 错误与中断。
		//
		// 为什么这么分：把非零退出当失败，模型就得自己写 hack 绕开（实测：它
		// 会拼 `; echo "exit=$?"` 让整条命令以 0 退出，把失败塞进输出里），
		// 而"命令失败了"本来就该是**事实**、由模型自己判断要不要重试。
		out["output"] = exitMarker(res.Output, res.ExitCode)
	}
	return out, nil
}

// exitMarker 在输出末尾补上退出码标记（DSH 对位）。
func exitMarker(output string, code int) string {
	marker := fmt.Sprintf("[exit code: %d]", code)
	if output == "" {
		return marker
	}
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	return output + marker
}

var _ tool.Tool = (*Shell)(nil)
