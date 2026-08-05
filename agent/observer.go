// Package agent 提供智能体运行时：任务循环、会话管理与事件通知。
package agent

import "hegq.net/mos/llm"

// EventType 表示运行时事件的类型。
type EventType int

const (
	// EventStep 模型在一轮循环中的中间推理（思考文本，通常伴随工具调用）。
	EventStep EventType = iota
	// EventToolCall 模型请求调用工具。
	EventToolCall
	// EventToolResult 工具执行完成。
	EventToolResult
	// EventDone 任务完成，产出最终回复。
	EventDone
)

// Event 是运行时向外广播的事件，用于流式输出、中间过程观测等。
// Loop Engineering：通过 Iteration 追踪循环轮次，Step 事件暴露每轮思考。
type Event struct {
	Type       EventType
	SessionID  string
	Iteration  int    // 当前循环迭代序号（从 1 开始）
	Content    string // EventStep 的思考文本 / EventDone 的最终回复
	Delta      string // 预留：流式文本增量
	ToolCall   *llm.ToolCall
	ToolResult any
}

// Observer 是事件回调函数。
type Observer func(Event)
