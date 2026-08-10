package agent

import (
	"context"

	"hegq.net/mos/llm"
)

// Auditor 审计一次工具调用的结果，决定哪些"事实"可以被写入任务状态。
// 审计门（Audit Gate）：只有被环境独立验证过的事实才能更新任务状态，
// 模型"自称完成"的声明一律不算数——环境说了算。
type Auditor interface {
	Audit(ctx context.Context, call *llm.ToolCall, result any) ([]Fact, error)
}

// DefaultAuditor 是基于"只读白名单"的默认审计器：
// 只读/自证类工具（calculator、read_file、grep 等）的输出本身就是环境的回答，
// 视为已验证事实；变更类工具（shell、write_file 等）的输出属于"未核实声明"，
// 不写入任务状态，需后续通过只读工具二次取证。
type DefaultAuditor struct {
	Trusted map[string]bool
}

// NewDefaultAuditor 创建默认审计器，预置内置只读工具白名单。
func NewDefaultAuditor() *DefaultAuditor {
	return &DefaultAuditor{Trusted: defaultTrustedTools()}
}

// Trust 将自定义工具加入白名单（仅当该工具输出可被环境独立验证时）。
func (d *DefaultAuditor) Trust(name string) {
	if d.Trusted == nil {
		d.Trusted = map[string]bool{}
	}
	d.Trusted[name] = true
}

// defaultTrustedTools 返回内置只读工具白名单。
func defaultTrustedTools() map[string]bool {
	return map[string]bool{
		"calculator":   true,
		"current_time": true,
		"web_fetch":    true,
		"web_search":   true,
		"read_file":    true,
		"find_files":   true,
		"grep":         true,
	}
}

// Audit 实现 Auditor：白名单内工具的输出生成一条已验证事实，其余返回空。
func (d *DefaultAuditor) Audit(_ context.Context, call *llm.ToolCall, result any) ([]Fact, error) {
	if d == nil || d.Trusted == nil || !d.Trusted[call.Name] {
		return nil, nil
	}
	evidence := formatToolResult(result)
	return []Fact{{
		Summary:  call.Name + " → " + truncate(evidence, 120),
		Evidence: truncate(evidence, 512),
		Tool:     call.Name,
	}}, nil
}

// truncate 按 rune 截断字符串，避免截断多字节字符。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
