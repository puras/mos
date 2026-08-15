package event

import (
	"encoding/json"

	"hegq.net/mos/llm"
)

// 会话事件类型（durable，append-only 日志，见架构 v2 §3.2 / §4.1）。
// 事件类型是字符串常量，与 Observer 的 EventType（int 观测枚举）相互独立：
// 会话事件是唯一事实来源（持久化/回放），观测事件是 emit 投影（平台观测）。
const (
	SessionEventTurnStart         = "turn/start"
	SessionEventTurnEnd           = "turn/end"
	SessionEventStepStart         = "step/start"
	SessionEventStepEnd           = "step/end"
	SessionEventUserMessage       = "user/message"
	SessionEventAssistantChunk    = "assistant/chunk"
	SessionEventAssistantMessage  = "assistant/message"
	SessionEventToolCall          = "tool/call"
	SessionEventToolResult        = "tool/result"
	SessionEventCompactionStart   = "compaction/start"
	SessionEventCompactionSummary = "compaction/summary"
	SessionEventCompactionEnd     = "compaction/end"
	SessionEventPlanStart         = "plan/start"
	SessionEventPlanStep          = "plan/step"
	SessionEventPlanEnd           = "plan/end"
	SessionEventReflectionStart   = "reflection/start"
	SessionEventReflectionReview  = "reflection/review"
	SessionEventReflectionEnd     = "reflection/end"
)

// ---- 事件载荷（typed payload）----

// TurnData 一轮任务的打开/关闭信息。
type TurnData struct {
	Turn   int    `json:"turn"`
	Reason string `json:"reason,omitempty"` // user / continuation / inject ...
}

// StepData 一次模型调用 + 其触发的工具执行。
type StepData struct {
	Turn int `json:"turn"`
	Step int `json:"step"`
}

// UserMessageData 用户输入（含 inject 注入、目标延续等来源标注）。
type UserMessageData struct {
	Message string `json:"message"`
	Source  string `json:"source,omitempty"`
}

// AssistantChunkData 流式增量（token 级回放保真）。
type AssistantChunkData struct {
	Turn      int    `json:"turn"`
	Delta     string `json:"delta"`
	Reasoning string `json:"reasoning,omitempty"` // 推理内容增量（thinking 模式回放保真）
}

// AssistantMessageData 单步完整回复 + 用量。
type AssistantMessageData struct {
	Turn      int            `json:"turn"`
	Content   string         `json:"content"`
	Reasoning string         `json:"reasoning,omitempty"` // thinking 模式须原样回传
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
	Usage     llm.Usage      `json:"usage,omitempty"`
}

// ToolCallData 模型发起的工具调用请求。
type ToolCallData struct {
	Turn   int    `json:"turn"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Args   string `json:"args"`
}

// ToolResultData 工具执行结果：Result 为原始返回值（审计/平台观测用），
// Text 为给模型看的文本（截断后）；deriveMessages 优先采用 Text。
type ToolResultData struct {
	Turn   int    `json:"turn"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Result any    `json:"result,omitempty"`
	Text   string `json:"text,omitempty"`
	Error  string `json:"error,omitempty"`
}

// CompactionStartData 压缩开始（带锁语义的简化：单 Session 串行写入天然带锁）。
type CompactionStartData struct {
	Turn int `json:"turn"`
}

// CompactionEndData 压缩结束。
type CompactionEndData struct {
	Turn int `json:"turn"`
}

// CompactionSummaryData 压缩摘要（三段式 compaction/summary 事件）。
// ShadowedFrom/To 为被摘要替换的事件日志序号范围（含），可回放。
type CompactionSummaryData struct {
	Summary      string `json:"summary"`
	ShadowedFrom int64  `json:"shadowed_from"`
	ShadowedTo   int64  `json:"shadowed_to"`
}

// PlanStartData 计划阶段开始（Plan&Execute）：目标 + 步骤数。
type PlanStartData struct {
	Turn  int    `json:"turn"`
	Goal  string `json:"goal"`
	Steps int    `json:"steps"`
}

// PlanStepData 计划中一步的状态迁移（pending → running → done/failed）。
type PlanStepData struct {
	Turn    int    `json:"turn"`
	Index   int    `json:"index"`
	Summary string `json:"summary"`
	Status  string `json:"status"` // pending / running / done / failed
}

// PlanEndData 计划阶段结束：completed（全部步骤通过校验）或 failed。
type PlanEndData struct {
	Turn   int    `json:"turn"`
	Status string `json:"status"` // completed / failed
	Detail string `json:"detail,omitempty"`
}

// ReflectionStartData 反思循环开始（Basic Reflection）：目标 + 最大迭代次数。
type ReflectionStartData struct {
	Turn          int    `json:"turn"`
	Goal          string `json:"goal"`
	MaxIterations int    `json:"max_iterations"`
}

// ReflectionReviewData 一轮评审结果（Issues 为空 = 评审通过）。
type ReflectionReviewData struct {
	Turn      int      `json:"turn"`
	Iteration int      `json:"iteration"`
	Issues    []string `json:"issues"`
	Score     int      `json:"score,omitempty"`
	Passed    bool     `json:"passed"`
}

// ReflectionEndData 反思循环结束：completed / max_iterations / failed。
type ReflectionEndData struct {
	Turn      int    `json:"turn"`
	Iteration int    `json:"iteration"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
}

// MarshalData 将载荷序列化为 SessionEvent.Data。
func MarshalData(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}
