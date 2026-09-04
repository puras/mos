// Package agent 提供智能体运行时概念：事件/观测定义与扩展点接口
// （Audit/Compaction/Truncation/RAG/SystemAssembler）；任务循环与驱动见
// 子包 agent/driver、agent/driver/react。
package agent

import (
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
)

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
	// EventStream 模型流式输出的文本增量（Delta 承载正文，Reasoning 承载思考增量）。
	EventStream
	// EventCompacted 历史被 fresh-context 压缩（M2.5）：Content 为压缩摘要文本。
	EventCompacted
	// EventContextUsage 上下文利用率观测（M2.5）：Content 为 "N%"，Usage 承载本次用量。
	EventContextUsage
	// EventKnowledgeGap 知识差距（M2.7 Agentic RAG）：检索有候选但全部低于知识守门
	// 最低分，Content 为查询，交由模型经外部工具补齐。
	EventKnowledgeGap
	// EventPlan 计划产出（Plan&Execute）：Content 为步骤摘要文本。
	EventPlan
	// EventReflection 评审结果（Basic Reflection）：Content 为问题摘要文本。
	EventReflection
	// EventScript 脚本产出（Scripted 驱动）：Content 为脚本摘要文本。
	EventScript
	// EventResume 从 checkpoint 恢复（M2.8）：Content 为恢复说明（挂起轮续跑）。
	EventResume
	// EventDisposed 句柄释放（M2.8）：AgentHandle.Dispose 广播。
	EventDisposed
	// EventSkillLoad 技能清单装载（skill/load 事件投影）。
	EventSkillLoad
	// EventSkillActivate 技能激活（skill/activate 事件投影）。
	EventSkillActivate
	// EventSkillDeactivate 技能取消激活（skill/deactivate 事件投影）。
	EventSkillDeactivate
)

// Event 是运行时向外广播的事件，用于流式输出、中间过程观测等。
// Loop Engineering：通过 Turn 追踪循环轮次（一个 Turn = 一次模型调用 + 其触发的所有工具执行），Step 事件暴露每轮思考。
type Event struct {
	Type       EventType
	SessionID  string
	Turn       int    // 当前 Turn 序号（从 1 开始）
	Content    string // EventStep 的思考文本 / EventDone 的最终回复 / EventCompacted 摘要 / EventContextUsage 利用率
	Delta      string // 预留：流式文本增量
	Reasoning  string // 流式推理内容增量（EventStream）
	ToolCall   *llm.ToolCall
	ToolResult any
	Fact       *session.Fact // EventFact：审计通过的已验证事实
	Usage      llm.Usage     // EventContextUsage：模型 token 用量
}

// Observer 是事件回调函数。
type Observer func(Event)
