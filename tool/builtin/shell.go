package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/puras/mos/sandbox"
	"github.com/puras/mos/tool"
)

// Shell 执行一条 shell 命令（经 sandbox 隔离执行）。
//
// 三条纪律：
//   - **fail-closed**：没装配 sandbox.Runner 就不执行——不存在"没配沙箱就跑裸
//     命令"的默认路径（归档曾有该缺陷，用兜底 Runner 修）。
//   - **命令失败是事实不是故障**：非零退出码以 IsError 回填，但输出照样给模型
//     （它要靠输出自我纠正）。
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
		"非零退出码视为失败，输出照样返回。输出过长会被截断，" +
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
		// 命令失败：回填给模型（它要靠输出自我纠正）。结构化字段随内容一起给。
		return nil, fmt.Errorf("命令失败（退出码 %d）:\n%s%s", res.ExitCode, res.Output, s.spillHint(out))
	}
	return out, nil
}

// spillHint 在失败消息里附上落盘路径（截断时）。
func (s *Shell) spillHint(out map[string]any) string {
	if p, ok := out["output_path"].(string); ok && p != "" {
		return fmt.Sprintf("\n[输出已截断，全文见 %s]", p)
	}
	return ""
}

var _ tool.Tool = (*Shell)(nil)
