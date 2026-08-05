// Package planner 提供 Agent 决策规划策略的抽象与实现。
package planner

import (
	"context"
	"fmt"

	"hegq.net/mos/llm"
)

// Action 是规划器在任务循环中作出的一个决策。
type Action struct {
	// Content 是模型生成的回复文本；ToolCall 非空时是伴随的思考文本。
	Content string
	// ToolCall 非空表示需要调用该工具，为空表示直接作答（任务结束）。
	ToolCall *llm.ToolCall
}

// Planner 在任务循环中决定下一步动作。
// history 为当前完整对话（含 system 引导）；tools 为可调用的工具定义。
type Planner interface {
	NextAction(ctx context.Context, history []llm.Message, tools []llm.Tool) (Action, error)
}

// ReAct 是默认的 ReAct（Reason + Act）策略：
// 调用模型获取回复，若模型请求调用工具则返回 ToolCall，否则返回最终作答。
type ReAct struct {
	provider    llm.Provider
	instruction string // 可选的思考引导语，注入为 system 消息
}

// NewReAct 创建 ReAct 规划器。
func NewReAct(provider llm.Provider, opts ...ReActOption) *ReAct {
	p := &ReAct{provider: provider}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// ReActOption 是 ReAct 的配置项。
type ReActOption func(*ReAct)

// WithInstruction 设置思考引导语（如 ReAct 提示词），不污染用户历史。
func WithInstruction(s string) ReActOption {
	return func(p *ReAct) { p.instruction = s }
}

// NextAction 实现 planner.Planner。
func (p *ReAct) NextAction(ctx context.Context, history []llm.Message, tools []llm.Tool) (Action, error) {
	req := &llm.Request{Messages: history, Tools: tools}
	if p.instruction != "" {
		msgs := make([]llm.Message, 0, len(history)+1)
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: p.instruction})
		req.Messages = append(msgs, history...)
	}

	resp, err := p.provider.Chat(ctx, req)
	if err != nil {
		return Action{}, fmt.Errorf("planner: chat: %w", err)
	}
	action := Action{Content: resp.Message.Content}
	if len(resp.Message.ToolCalls) > 0 {
		action.ToolCall = &resp.Message.ToolCalls[0]
	}
	return action, nil
}
