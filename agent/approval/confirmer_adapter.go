package approval

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// errNilConfirmer 表示 ConfirmerAdapter 被 nil Confirmer 构造。
var errNilConfirmer = errors.New("approval: ConfirmerAdapter with nil Confirmer")

// Confirmer 是旧的同步确认接口（M2.8 前，assistant 包）。保留为兼容层，
// 通过 ConfirmerAdapter 桥接为异步 Approval。新代码应直接实现 Approval。
type Confirmer interface {
	Confirm(ctx context.Context, req ConfirmRequest) (bool, error)
}

// ConfirmRequest 是一次危险操作的人工确认请求（与 assistant.ConfirmRequest 同构，
// 避免 import cycle：assistant 会 import approval，approval 不得回引 assistant）。
type ConfirmRequest struct {
	ToolName string
	Command  string
	Reason   string
}

// ConfirmerAdapter 将同步 Confirmer 适配为异步 Approval（保留旧 CLI y/N 体验）：
//   - Request 同步调用 inner.Confirm，返回 ID 并把决策存入内部表（one-shot）；
//   - 实现 Immediate：驱动识别到决策立即可用时直接应用、不挂起——
//     等价于旧版同步确认语义（确认完成即放行，无需人工二次驱动）。
//
// 注意：ConfirmerAdapter 只适用于"确认本身是快速同步调用"的场景（CLI y/N）；
// 真正的异步人工介入（Web 弹窗 / 审批流）应直接实现 Approval。
type ConfirmerAdapter struct {
	inner Confirmer

	mu     sync.Mutex
	table  map[ApprovalID]ApprovalDecision
}

// NewConfirmerAdapter 包装一个同步 Confirmer。
func NewConfirmerAdapter(c Confirmer) *ConfirmerAdapter {
	return &ConfirmerAdapter{inner: c, table: make(map[ApprovalID]ApprovalDecision)}
}

// Request 同步调用 inner.Confirm；approved 打包为 ApprovalDecision 存入内部表。
// 返回生成的 ID；驱动经 Immediate.Decision 立即取回，不挂起。
func (a *ConfirmerAdapter) Request(ctx context.Context, req ApprovalRequest) (ApprovalID, error) {
	if a == nil || a.inner == nil {
		return "", errNilConfirmer
	}
	ok, err := a.inner.Confirm(ctx, ConfirmRequest{
		ToolName: req.ToolName,
		Command:  req.Command,
		Reason:   req.Reason,
	})
	if err != nil {
		return "", err
	}
	id := ApprovalID(uuid.NewString())
	a.mu.Lock()
	a.table[id] = ApprovalDecision{Approved: ok, DecidedAt: time.Now()}
	a.mu.Unlock()
	return id, nil
}

// Decision 实现 Immediate：返回内部表里的决策（Request 已完成，立即可用）。
func (a *ConfirmerAdapter) Decision(ctx context.Context, id ApprovalID) (ApprovalDecision, bool) {
	if a == nil {
		return ApprovalDecision{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.table[id]
	return d, ok
}

// Resolve 立即返回 nil：ConfirmerAdapter 在 Request 时已同步确认，无异步回填。
func (a *ConfirmerAdapter) Resolve(ctx context.Context, id ApprovalID, d ApprovalDecision) error {
	return nil
}
