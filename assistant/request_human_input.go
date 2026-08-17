package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/tool"
)

// requestHumanParams 是 request_human_input 工具参数：question 必填，context 供平台展示。
type requestHumanParams struct {
	Question string `json:"question"`
	Context  string `json:"context,omitempty"`
}

// RequestHumanInput 是 M2.8 人工介入工具：模型主动请求人类输入 / 确认 / 授权
// （对齐 PI SuspendedOperation + 规划 M2.8 request_human_input）。
//
// 执行语义：
//   - 首次调用：经 ctx 读取审批通道（Approval.Request）→ 返回 *approval.SuspendError
//     挂起（checkpoint 落库，driver 退出 Run）；人类在平台侧作答 → Resolve；
//   - resume 恢复：经 ctx 读取决策表，该调用已有回答 → 直接返回回答（工具结果）；
//     已拒绝 → 返回错误（原因反馈给模型）。
//
// 依赖 ctx 注入：approval.WithApproval（审批通道）、tool.WithCallID（当前调用 ID）、
// approval.WithResumeDecisions（resume 决策表）——均由驱动在工具执行前注入。
// 未配置审批通道时返回明确错误（fail-closed）。
type RequestHumanInput struct{}

// Name 实现 tool.Tool。
func (RequestHumanInput) Name() string { return "request_human_input" }

// Description 实现 tool.Tool。
func (RequestHumanInput) Description() string {
	return "请求人类输入 / 确认 / 授权：暂停当前执行并等待人类回答，回答以工具结果返回（含 answered_by）。适用于需要用户提供信息、确认高风险操作或授权外部动作的场景。"
}

// Parameters 实现 tool.Tool。
func (RequestHumanInput) Parameters() any { return &requestHumanParams{} }

// Execute 实现 tool.Tool。
func (t RequestHumanInput) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[requestHumanParams](args)
	if err != nil {
		return nil, fmt.Errorf("request_human_input: %w", err)
	}
	question := strings.TrimSpace(p.Question)
	if question == "" {
		return nil, fmt.Errorf("request_human_input: question 不能为空")
	}
	callID := tool.CallIDFrom(ctx)

	// resume 决策表：该调用已有回答 → 直接返回（不重新发起）
	if decs := approval.ResumeDecisions(ctx); decs != nil {
		if d, ok := decs[callID]; ok {
			if !d.Approved {
				reason := d.Reason
				if reason == "" {
					reason = "人类拒绝了请求"
				}
				return nil, fmt.Errorf("request_human_input: %s", reason)
			}
			return map[string]any{
				"answer":     d.Content,
				"answered_by": d.DecidedBy,
			}, nil
		}
	}

	// 审批通道
	ap, ok := approval.ApprovalFrom(ctx)
	if !ok || ap == nil {
		return nil, fmt.Errorf("request_human_input: 未配置 Approval（人工确认通道），无法请求人类输入（fail-closed）")
	}
	req := approval.ApprovalRequest{
		ToolName: "request_human_input",
		Command:  question,
		Reason:   p.Context,
		Args:     string(args),
	}
	id, err := ap.Request(ctx, req)
	if err != nil {
		return nil, err
	}
	// 同步确认器（Immediate）：决策立即可用，直接返回回答、不挂起
	if im, ok := ap.(approval.Immediate); ok {
		if d, found := im.Decision(ctx, id); found {
			if !d.Approved {
				return nil, fmt.Errorf("request_human_input: 人类拒绝了请求: %s", d.Reason)
			}
			return map[string]any{
				"answer":     d.Content,
				"answered_by": d.DecidedBy,
			}, nil
		}
	}
	// 异步：挂起（driver 检测 SuspendError → checkpoint 落库 → 退出 Run）
	return nil, &approval.SuspendError{
		ID:         id,
		Request:    req,
		CallID:     callID,
		HookKind:   hook.HookToolExecute,
		ReplayTurn: 0, // 由 driver 在 saveCheckpoint 时用当前 turn 填充
		ReplayStep: 0,
	}
}

var _ tool.Tool = RequestHumanInput{}
