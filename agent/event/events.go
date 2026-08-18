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
	SessionEventScriptStart       = "script/start"
	SessionEventScriptStep        = "script/step"
	SessionEventScriptEnd         = "script/end"
	SessionEventReflectionStart   = "reflection/start"
	SessionEventReflectionReview  = "reflection/review"
	SessionEventReflectionEnd     = "reflection/end"

	// ---- Agent 域（live，编排与生命周期；架构 v2 §4.2）----

	// SessionEventApprovalRequest 异步审批发起（serial 决策 / emit 观测）：
	// 钩子（tool/pre-execute 或工具内）触发 Approval.Request 后写入；后续
	// Approval.Resolve(id, decision) 回填 decision → 驱动从 checkpoint 恢复。
	SessionEventApprovalRequest = "approval/request"
	// SessionEventApprovalResolve 异步审批回填（emit 观测）：决策结果落地，
	// 不影响权威状态（驱动续跑以 checkpoint.Decision 字段为准）。
	SessionEventApprovalResolve = "approval/resolve"
	// SessionEventCheckpointSave checkpoint 落库（durable）：driver 在任意钩子点
	// 抛 SuspendError 前写入；事件载荷含快照 + 钩子点 + 待定决策。
	SessionEventCheckpointSave = "checkpoint/save"
	// SessionEventCheckpointRestore checkpoint 恢复（durable）：Handle.Resume
	// 加载 checkpoint 后写入，标记续跑起点（ReplayTurn/ReplayStep）。
	SessionEventCheckpointRestore = "checkpoint/restore"
	// SessionEventAgentCreated Agent 句柄创建（emit，Registry.Create）。
	SessionEventAgentCreated = "agent/created"
	// SessionEventAgentDisposed Agent 句柄释放（emit，Handle.Dispose 走 Scope.Close）。
	SessionEventAgentDisposed = "agent/disposed"
	// SessionEventAgentStatus Agent 状态迁移（emit）：running / idle / blocked / suspended。
	SessionEventAgentStatus = "agent/status"
	// SessionEventSubagentStart SubAgent 生命周期开始（M3 规划，本里程碑仅占位）。
	SessionEventSubagentStart = "subagent/start"
	// SessionEventAgentInbox Agent 输入入队/claim（M3 Inbox 规划，本里程碑仅占位）。
	SessionEventAgentInbox = "agent/inbox"
	// SessionEventSkillLoad 技能清单装载（emit 观测）：defaultSystemAssembler
	// 首次扫描后写入一次（durable），载荷 SkillLoadData 含全部 Skill frontmatter 摘要。
	SessionEventSkillLoad = "skill/load"
	// SessionEventSkillActivate 技能激活（emit 观测）：activate_skill 工具执行成功后写入。
	SessionEventSkillActivate = "skill/activate"
	// SessionEventSkillDeactivate 技能取消激活（emit 观测）：每轮 runTurnInner 末尾统一写入。
	SessionEventSkillDeactivate = "skill/deactivate"
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

// ScriptStartData 脚本阶段开始（Scripted 驱动）：目标 + 步骤数。
type ScriptStartData struct {
	Turn  int    `json:"turn"`
	Goal  string `json:"goal"`
	Steps int    `json:"steps"`
}

// ScriptStepData 脚本中一步的状态迁移（pending → running → done/failed/denied/skipped）。
type ScriptStepData struct {
	Turn   int    `json:"turn"`
	StepID string `json:"step_id"`
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// ScriptEndData 脚本阶段结束：completed / failed + 每步状态 + 命名输出摘要（架构 v2 §6）。
// Steps / Outputs 是脚本执行报告（scriptReport）的可观测投影，UI 与审计可直接读取。
type ScriptEndData struct {
	Turn    int               `json:"turn"`
	Status  string            `json:"status"`
	Detail  string            `json:"detail,omitempty"`
	Steps   map[string]string `json:"steps,omitempty"`   // stepID → done/failed/skipped/denied
	Outputs map[string]any    `json:"outputs,omitempty"` // stepID → 命名输出
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

// ---- M2.8 Agent 域 / Approval / Checkpoint 载荷 ----

// ApprovalRequestData 异步审批发起载荷（approval/request 事件）。
// ID 由 Approval.Request 生成，平台侧 Resolve 时回传；HookKind 指明发起位置
// （如 tool/pre-execute），便于审计与 UI 分组。
type ApprovalRequestData struct {
	ID       string `json:"id"`
	ToolName string `json:"tool_name"`
	Command  string `json:"command"`
	Reason   string `json:"reason"`
	Args     string `json:"args,omitempty"`
	HookKind string `json:"hook_kind"` // hook.HookKind 字符串值
}

// ApprovalResolveData 异步审批回填载荷（approval/resolve 事件）。
// 仅作观测/审计；权威状态以 checkpoint.Decision 字段为准（持久化在 Store 里）。
type ApprovalResolveData struct {
	ID        string `json:"id"`
	Approved  bool   `json:"approved"`
	Reason    string `json:"reason,omitempty"`
	DecidedBy string `json:"decided_by,omitempty"`
}

// CheckpointData checkpoint 落库/恢复事件载荷（checkpoint/save 与 checkpoint/restore 共用）。
// 持久化由 agent/checkpoint.Store 负责；事件作为 durable 观测索引。
type CheckpointData struct {
	Turn       int    `json:"turn"`
	Step       int    `json:"step"`
	HookKind   string `json:"hook_kind"`
	DecisionOK bool   `json:"decision_ok,omitempty"` // restore 时携带 Resolve 结果
}

// AgentHandleData Agent 句柄生命周期事件载荷（agent/created / agent/disposed / agent/status）。
// Status 字段取值：created / running / idle / blocked / suspended / disposed。
type AgentHandleData struct {
	HandleID  string `json:"handle_id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status,omitempty"` // agent/status 专用
	Reason    string `json:"reason,omitempty"`
}

// SkillLoadData 技能清单装载载荷（skill/load 事件）。
type SkillLoadData struct {
	Skills []SkillMeta `json:"skills"`
}

// SkillMeta 技能 frontmatter 摘要（事件载荷用，避免重复正文）。
type SkillMeta struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	SchemaVersion string   `json:"schema_version,omitempty"`
	WhenToUse     string   `json:"when_to_use,omitempty"`
	AllowedTools  []string `json:"allowed_tools,omitempty"`
	References    []string `json:"references,omitempty"`
	Paths         []string `json:"paths,omitempty"`
	Source        string   `json:"source,omitempty"`
	UserInvocable bool     `json:"user_invocable,omitempty"`
}

// SkillActivateData 技能激活载荷（skill/activate 事件）。
// 仅记元数据；正文由 activate_skill 工具直接返回给模型（不进 Event Log）。
type SkillActivateData struct {
	Name         string   `json:"name"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	References   []string `json:"references,omitempty"`
}

// SkillDeactivateData 技能取消激活载荷（skill/deactivate 事件）。
type SkillDeactivateData struct {
	Names []string `json:"names"`
}

// MarshalData 将载荷序列化为 SessionEvent.Data。
func MarshalData(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}
