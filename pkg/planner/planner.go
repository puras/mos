// Package planner 提供 Agent 决策规划策略的抽象与实现。
package planner

import (
	"context"
	"errors"
	"fmt"

	"hegq.net/mos/pkg/llm"
)

// Action 是规划器在任务循环中作出的一个决策。
type Action struct {
	// Content 是模型生成的回复文本；ToolCall 非空时是伴随的思考文本。
	Content string
	// ReasoningContent 是伴随的推理内容（思维链，thinking 模式多轮须回传）。
	ReasoningContent string
	// ToolCall 非空表示需要调用该工具，为空表示直接作答（任务结束）。
	ToolCall *llm.ToolCall
}

// Planner 在任务循环中决定下一步动作。
// history 为当前完整对话（含 system 引导）；tools 为可调用的工具定义。
type Planner interface {
	NextAction(ctx context.Context, history []llm.Message, tools []llm.Tool) (Action, error)
}

// StreamingPlanner 是支持流式产出的规划器：
// 模型产出过程中的文本增量经 emit 实时回调（供应用层实现打字机效果），
// 返回的 Action 与 NextAction 语义一致（增量聚合后的完整结果）。
type StreamingPlanner interface {
	NextActionStream(ctx context.Context, history []llm.Message, tools []llm.Tool, emit func(llm.StreamChunk) error) (Action, error)
}

// ReAct 是默认的 ReAct（Reason + Act）策略：
// 调用模型获取回复，若模型请求调用工具则返回 ToolCall，否则返回最终作答。
type ReAct struct {
	model       llm.ChatModel
	instruction string // 可选的思考引导语，注入为 system 消息
}

// NewReAct 创建 ReAct 规划器。
func NewReAct(model llm.ChatModel, opts ...ReActOption) *ReAct {
	p := &ReAct{model: model}
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
	resp, err := p.model.Chat(ctx, p.buildRequest(history, tools))
	if err != nil {
		return Action{}, fmt.Errorf("planner: chat: %w", err)
	}
	return toAction(resp), nil
}

// NextActionStream 实现 planner.StreamingPlanner。
// 模型声明流式能力时经 ChatStream 实时推送增量；协议不支持（ErrUnsupported）时
// 回退非流式 Chat，并在 emit 一次性补齐完整内容，应用层按同一事件流处理。
func (p *ReAct) NextActionStream(ctx context.Context, history []llm.Message, tools []llm.Tool, emit func(llm.StreamChunk) error) (Action, error) {
	req := p.buildRequest(history, tools)
	if p.model.Capabilities().HasCapability(llm.CapStreaming) {
		resp, err := p.model.ChatStream(ctx, req, emit)
		if err != nil && !errors.Is(err, llm.ErrUnsupported) {
			return Action{}, fmt.Errorf("planner: chat stream: %w", err)
		}
		if err == nil {
			return toAction(resp), nil
		}
		// ErrUnsupported：声明了能力但实现未支持，回退非流式
	}
	resp, err := p.model.Chat(ctx, req)
	if err != nil {
		return Action{}, fmt.Errorf("planner: chat: %w", err)
	}
	if emit != nil && resp.Message.Content != "" {
		emit(llm.StreamChunk{Delta: resp.Message.Content})
	}
	return toAction(resp), nil
}

// buildRequest 组装模型请求：注入可选的思考引导语（system 消息，不污染用户历史）。
func (p *ReAct) buildRequest(history []llm.Message, tools []llm.Tool) *llm.Request {
	req := &llm.Request{Messages: history, Tools: tools}
	if p.instruction != "" {
		msgs := make([]llm.Message, 0, len(history)+1)
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: p.instruction})
		req.Messages = append(msgs, history...)
	}
	return req
}

// toAction 将模型响应转换为规划器决策。
func toAction(resp *llm.Response) Action {
	action := Action{Content: resp.Message.Content, ReasoningContent: resp.Message.ReasoningContent}
	if len(resp.Message.ToolCalls) > 0 {
		action.ToolCall = &resp.Message.ToolCalls[0]
	}
	return action
}
