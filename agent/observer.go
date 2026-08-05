// Package agent 提供智能体运行时：任务循环、会话管理与事件通知。
package agent

import "github.com/puras/mos/llm"

// EventType 表示运行时事件的类型。
type EventType int

const (
	// EventToolCall 模型请求调用工具。
	EventToolCall EventType = iota
	// EventToolResult 工具执行完成。
	EventToolResult
	// EventDone 任务完成，产出最终回复。
	EventDone
)

// Event 是运行时向外广播的事件，用于流式输出、中间过程观测等。
type Event struct {
	Type       EventType
	SessionID  string
	Content    string // EventDone 时的最终回复
	Delta      string // 预留：流式文本增量
	ToolCall   *llm.ToolCall
	ToolResult any
}

// Observer 是事件回调函数。
type Observer func(Event)
