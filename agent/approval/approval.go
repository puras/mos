// Package approval 提供异步审批通道（架构 v2 §3.4 approval + §10.6 PI SuspendedOperation）。
// 驱动层在 tool/pre-execute 守卫内发起 Approval.Request，返回 *SuspendError 退出 Run；
// 平台异步 Resolve(id, decision) 后经 AgentHandle.Resume 续跑。one-shot 由实现强制；
// 未配置 Approval 时对需要确认的操作 fail-closed 拒绝。Confirmer 兼容层
// ConfirmerAdapter 通过 Immediate 接口保留旧 CLI y/N 同步体验。
package approval

import (
	"context"
	"errors"
	"time"

	"github.com/puras/mos/agent/hook"
)

// ApprovalID 是异步审批通道的全局唯一标识（UUID 字符串）。
// 平台侧 Resolve(id, decision) 时回传；驱动通过 SuspendError.ID 关联。
type ApprovalID string

// ApprovalRequest 是一次审批请求的载荷（approval/request 事件载荷来源）。
// Args 是工具调用参数 JSON 原文，供平台展示上下文；不参与驱动逻辑。
type ApprovalRequest struct {
	ToolName string `json:"tool_name"`
	Command  string `json:"command"`
	Reason   string `json:"reason"`
	Args     string `json:"args,omitempty"`
}

// ApprovalDecision 是一次审批回填的决策（approval/resolve 事件载荷 + Checkpoint.Decisions 值）。
// Approved=true 时驱动放行；false 时该工具调用显式失败回填（fail-closed 默认拒绝）。
// Content 承载人类自由文本回答（request_human_input 工具：回答即工具结果）；普通确认（放行/拒绝）
// 仅用 Approved/Reason。
type ApprovalDecision struct {
	Approved  bool      `json:"approved"`
	Reason    string    `json:"reason,omitempty"`
	Content   string    `json:"content,omitempty"` // request_human_input 的回答
	DecidedBy string    `json:"decided_by,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// Approval 是异步审批通道接口（架构 v2 §3.4 approval seam）：
//   - Request 发起审批，返回 ID；不阻塞 driver，driver 拿到 ID 后抛 SuspendError 退出 Run。
//   - Resolve 由平台在收到用户决定后调用；与 Request 配对，一次性（one-shot）。
//
// 默认内存实现 InMemApproval 见 inmem.go；Confirmer 兼容层 ConfirmerAdapter 见
// confirmer_adapter.go。平台可实现 Web / Redis / 数据库后端（持久化 Resume 决策）。
type Approval interface {
	Request(ctx context.Context, req ApprovalRequest) (ApprovalID, error)
	Resolve(ctx context.Context, id ApprovalID, d ApprovalDecision) error
}

// Immediate 是可选接口：Request 后立即可获得决策（同步确认器适配，ConfirmerAdapter 实现）。
// 驱动识别到该接口且决策立即可用时直接应用决策、不挂起——保留旧 CLI y/N 同步体验。
type Immediate interface {
	Decision(ctx context.Context, id ApprovalID) (ApprovalDecision, bool)
}

// RequiredError 是策略检查器返回的"需要人工确认"标记（非硬拒绝）。
// 驱动层 policyGuard 识别后发起 Approval.Request；未配置 Approval 时 fail-closed 拒绝。
// 区别于普通策略错误（路径越界等硬拒绝，直接拦截回填）。
type RequiredError struct {
	Request ApprovalRequest
}

// Error 实现 error 接口。
func (e *RequiredError) Error() string {
	return "approval required for tool " + e.Request.ToolName
}

// ErrUnknownID 标识 Resolve 未知 ID；Resolve 应被幂等处理（不 panic）。
var ErrUnknownID = errors.New("approval: unknown id")

// ErrAlreadyResolved 标识同一 ID 的二次 Resolve（one-shot 被破坏）。
var ErrAlreadyResolved = errors.New("approval: already resolved")

// ---- 上下文辅助（request_human_input 等工具经 ctx 读取审批通道与 resume 决策）----

type approvalCtxKey struct{}

// WithApproval 将审批通道放入上下文（驱动在工具执行前注入）。
func WithApproval(ctx context.Context, a Approval) context.Context {
	return context.WithValue(ctx, approvalCtxKey{}, a)
}

// ApprovalFrom 从上下文读取审批通道（未注入时 ok=false）。
func ApprovalFrom(ctx context.Context) (Approval, bool) {
	a, ok := ctx.Value(approvalCtxKey{}).(Approval)
	return a, ok
}

type resumeDecisionsCtxKey struct{}

// WithResumeDecisions 将 resume 决策表（tool call ID → 决策）放入上下文，
// 供 request_human_input 工具恢复时读取人类回答。
func WithResumeDecisions(ctx context.Context, decisions map[string]ApprovalDecision) context.Context {
	return context.WithValue(ctx, resumeDecisionsCtxKey{}, decisions)
}

// ResumeDecisions 从上下文读取 resume 决策表（未注入时 nil）。
func ResumeDecisions(ctx context.Context) map[string]ApprovalDecision {
	m, _ := ctx.Value(resumeDecisionsCtxKey{}).(map[string]ApprovalDecision)
	return m
}

// SuspendError 是 driver 抛出的挂起信号（架构 v2 §3.4 异步 approval）：
// 携带审批 ID + 请求上下文 + 钩子点 + 重放位置（turn/step）+ 挂起工具调用 ID。
// 驱动层在批量守卫预检返回后，经 errors.As 检测即写 checkpoint + 冒泡退出 Run。
// 叶子错误，不实现 Unwrap。
type SuspendError struct {
	ID         ApprovalID
	Request    ApprovalRequest
	CallID     string    // 挂起的工具调用 ID（checkpoint 恢复时按此键控决策表）
	HookKind   hook.HookKind
	ReplayTurn int // checkpoint 落库时的 turn
	ReplayStep int // checkpoint 落库时的 step
}

// Error 实现 error 接口。
func (e *SuspendError) Error() string {
	return "agent: suspended awaiting approval " + string(e.ID) +
		" (tool=" + e.Request.ToolName + ", hook=" + string(e.HookKind) +
		", turn=" + itoa(e.ReplayTurn) + ")"
}

// itoa 是 strconv.Itoa 的零依赖替代（避免 approval 包引入 strconv）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
