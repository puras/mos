// Package agent 提供智能体运行时：任务循环、会话管理与事件通知。
package agent

import "hegq.net/mos/pkg/llm"

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
	// EventFact 审计器验证通过，任务状态新增一条已验证事实。
	EventFact
	// EventStream 模型流式输出的文本增量（Delta 字段承载）。
	EventStream
)

// Event 是运行时向外广播的事件，用于流式输出、中间过程观测等。
// Loop Engineering：通过 Turn 追踪循环轮次（一个 Turn = 一次模型调用 + 其触发的所有工具执行），Step 事件暴露每轮思考。
type Event struct {
	Type       EventType
	SessionID  string
	Turn       int    // 当前 Turn 序号（从 1 开始）
	Content    string // EventStep 的思考文本 / EventDone 的最终回复
	Delta      string // 预留：流式文本增量
	ToolCall   *llm.ToolCall
	ToolResult any
	Fact       *Fact // EventFact：审计通过的已验证事实
}

// Observer 是事件回调函数。
type Observer func(Event)
