// Package checkpoint 提供断点快照与恢复存储（架构 v2 §3.8 / §10.6 PI SuspendedOperation，
// 参考 LangChain checkpointer）：driver 在任意钩子点挂起（SuspendError）时写入 Checkpoint，
// AgentHandle.Resume 加载后从挂起轮续跑。Store 为可换后端 seam——SDK 内置内存与文件实现，
// sqlite / postgres 等数据库后端由业务层实现接口并注入（MoS 不操作数据库）。
package checkpoint

import (
	"context"
	"errors"
	"time"

	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
)

// Checkpoint 是一次挂起点的不可变快照（序列化落盘，恢复时反序列化）：
//   - Calls：挂起轮的工具调用批（resume 时跳过 LLM 调用，直接重新执行该批）；
//   - Request：触发挂起的审批请求（审计 / 平台展示）；
//   - SuspendedCallID + Decisions：resume 注入的决策表（approval guard 按 call ID 查表放行）。
type Checkpoint struct {
	SessionID string `json:"session_id"`
	Turn      int    `json:"turn"`
	Step      int    `json:"step"`
	HookKind  hook.HookKind `json:"hook_kind"`

	// Calls 是挂起轮的工具调用批（来自 assistant/message 的 ToolCalls）。
	Calls []llm.ToolCall `json:"calls"`
	// Request 是触发挂起的审批请求。
	Request approval.ApprovalRequest `json:"request"`
	// ApprovalID 是触发挂起的审批 ID（resume 时写 approval/resolve 事件关联）。
	ApprovalID approval.ApprovalID `json:"approval_id"`
	// SuspendedCallID 是被挂起的工具调用 ID（resume 时经 Decisions 键控放行）。
	SuspendedCallID string `json:"suspended_call_id"`
	// Decisions 是已回填决策，按 tool call ID 键控（Handle.Resume 注入；approval guard 查表）。
	Decisions map[string]approval.ApprovalDecision `json:"decisions,omitempty"`

	// Snapshot 是挂起时刻的 TaskState（已验证事实 / 状态机），resume 恢复任务进度。
	Snapshot session.TaskState `json:"snapshot"`

	// ScriptState 脚本执行状态快照（Scripted 驱动专用，可选）：
	// 脚本 + 已完成步骤输出 + 每步状态。resume 恢复 outputs/status 续跑。
	// 老 checkpoint（react/planexecute 等）无此字段，照常可读、恢复语义不变。
	ScriptState *planner.ScriptState `json:"script_state,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Store 是 checkpoint 的持久化后端（架构 v2 §3.8，参考 LangChain checkpointer）：
// backend 可换——内存 / 文件由 SDK 内置，数据库后端由业务层实现注入。
type Store interface {
	// Save 写入一个 checkpoint（同 sessionID 后写覆盖先写）。
	Save(ctx context.Context, ckpt *Checkpoint) error
	// Load 读取指定会话的最新 checkpoint（单 turn 文件策略：会话级单文件）。
	// 无 checkpoint 时返回 (nil, ErrNotFound)。
	Load(ctx context.Context, sessionID string) (*Checkpoint, error)
	// Delete 删除指定会话的全部 checkpoint。
	Delete(ctx context.Context, sessionID string) error
}

// ErrNotFound 表示无 checkpoint 记录。
var ErrNotFound = errors.New("checkpoint: not found")
