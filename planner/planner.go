// Package planner 提供循环决策器的抽象与 ReAct 默认实现。
//
// 决策器只回答「下一步做什么」：把当前对话历史交给模型，产出一次 Action
// （直接作答，或请求若干工具调用）。工具的执行与回填属循环层（agent/driver），
// 决策器不碰工具。
package planner

import (
	"context"
	"errors"
	"fmt"

	"github.com/puras/mos/llm"
)

// Action 是决策器在一次循环中作出的决策。
type Action struct {
	// Content 是模型产出的文本。
	Content string
	// ReasoningContent 是思维链内容（推理模型扩展）；thinking 模式的多轮对话
	// 须原样回传，否则服务商报错——由循环层写入 assistant 事件并随投影带出。
	ReasoningContent string
	// ToolCalls 是模型请求调用的工具（完整保留，循环层负责执行与回填）。
	ToolCalls []llm.ToolCall
	// FinishReason 是模型停止原因；length/max_tokens 表示输出被截断，
	// 循环层据此对截断产生的工具调用 fail-closed（不执行）。
	FinishReason string
	// Usage 是本次决策的 token 用量（随 assistant 事件落会话日志）。
	Usage llm.Usage
}

// HasToolCalls 判断模型是否请求了工具调用。
func (a Action) HasToolCalls() bool { return len(a.ToolCalls) > 0 }

// Planner 决策器：循环中决定下一步动作。
// history 为当前完整对话（含系统引导），tools 为可调用的工具定义（可为空）。
type Planner interface {
	NextAction(ctx context.Context, history []llm.Message, tools []llm.Tool) (Action, error)
}

// StreamingPlanner 是支持流式产出的决策器：模型产出过程中的增量经 emit 推送，
// 返回的 Action 与 NextAction 语义一致（增量聚合后的完整结果）。
type StreamingPlanner interface {
	NextActionStream(ctx context.Context, history []llm.Message, tools []llm.Tool, emit func(llm.StreamChunk) error) (Action, error)
}

// ReAct 是默认决策器：模型直接作答；请求工具时把调用交循环层执行。
type ReAct struct {
	model llm.ChatModel
}

// NewReAct 创建 ReAct 决策器。model 为 nil 时循环层 fail-closed 拒绝运行。
func NewReAct(model llm.ChatModel) *ReAct { return &ReAct{model: model} }

// NextAction 实现 Planner（非流式）。
func (p *ReAct) NextAction(ctx context.Context, history []llm.Message, tools []llm.Tool) (Action, error) {
	resp, err := p.model.Chat(ctx, &llm.Request{Messages: history, Tools: tools})
	if err != nil {
		return Action{}, fmt.Errorf("planner: chat: %w", err)
	}
	return toAction(resp), nil
}

// NextActionStream 实现 StreamingPlanner：模型声明流式能力时经 ChatStream 实时
// 推送增量；未声明或声明但实现回退 ErrUnsupported 时改走非流式，并一次性补齐
// 完整内容——调用方按同一事件流处理，两条路径语义一致。
func (p *ReAct) NextActionStream(ctx context.Context, history []llm.Message, tools []llm.Tool, emit func(llm.StreamChunk) error) (Action, error) {
	req := &llm.Request{Messages: history, Tools: tools}
	if p.model.Capabilities().HasCapability(llm.CapStreaming) {
		resp, err := p.model.ChatStream(ctx, req, emit)
		if err == nil {
			return toAction(resp), nil
		}
		if !errors.Is(err, llm.ErrUnsupported) {
			return Action{}, fmt.Errorf("planner: chat stream: %w", err)
		}
	}
	resp, err := p.model.Chat(ctx, req)
	if err != nil {
		return Action{}, fmt.Errorf("planner: chat: %w", err)
	}
	if emit != nil && resp.Message.Content != "" {
		_ = emit(llm.StreamChunk{Delta: resp.Message.Content})
	}
	return toAction(resp), nil
}

// toAction 将模型响应转换为决策。
func toAction(resp *llm.Response) Action {
	return Action{
		Content:          resp.Message.Content,
		ReasoningContent: resp.Message.ReasoningContent,
		ToolCalls:        resp.Message.ToolCalls,
		FinishReason:     resp.FinishReason,
		Usage:            resp.Usage,
	}
}
