// Package planner 提供 Agent 决策规划策略的抽象与实现。
package planner

import (
	"context"
	"errors"
	"fmt"

	"github.com/puras/mos/llm"
)

// Action 是规划器在任务循环中作出的一个决策。
type Action struct {
	// Content 是模型生成的回复文本；ToolCall 非空时是伴随的思考文本。
	Content string
	// ReasoningContent 是伴随的推理内容（思维链，thinking 模式多轮须回传）。
	ReasoningContent string
	// ToolCalls 是模型请求调用的工具列表（完整保留；循环层负责执行与回填）。
	// 兼容单工具实现：仅有 ToolCall 时，循环层自动补齐为单元素列表。
	ToolCalls []llm.ToolCall
	// ToolCall 是 ToolCalls 的第一个元素（单工具快捷访问，保持既有语义）。
	ToolCall *llm.ToolCall
	// FinishReason 是本次决策的模型停止原因（length/max_tokens 表示输出被截断，
	// 循环层对截断产生的工具调用做 fail-closed 处理，不执行并显式失败回填）。
	FinishReason string
	// Usage 是本次决策的模型 token 用量（供上下文利用率监控等观测，M2.5）。
	Usage llm.Usage
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
	model llm.ChatModel
}

// NewReAct 创建 ReAct 规划器。
func NewReAct(model llm.ChatModel, opts ...ReActOption) *ReAct {
	p := &ReAct{model: model}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// ReActOption 是 ReAct 的配置项（扩展点，当前无内置选项）。
type ReActOption func(*ReAct)

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

// buildRequest 组装模型请求。
func (p *ReAct) buildRequest(history []llm.Message, tools []llm.Tool) *llm.Request {
	return &llm.Request{Messages: history, Tools: tools}
}

// toAction 将模型响应转换为规划器决策。
func toAction(resp *llm.Response) Action {
	action := Action{
		Content:          resp.Message.Content,
		ReasoningContent: resp.Message.ReasoningContent,
		Usage:            resp.Usage,
		FinishReason:     resp.FinishReason,
	}
	if calls := resp.Message.ToolCalls; len(calls) > 0 {
		action.ToolCalls = calls
		action.ToolCall = &calls[0]
	}
	return action
}
