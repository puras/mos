package approval

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// InMemApproval 是 Approval 的内存实现（架构 v2 §3.4）：单进程异步通道，
// 单测与本地调试用。生产环境由平台注入 Web / Redis 等后端实现。
//
// 并发安全。语义：
//   - Request 分配 ID 入 pending；Resolve 交付决策（pending → resolved，one-shot）；
//   - 二次 Resolve 返 ErrAlreadyResolved；未知 ID 返 ErrUnknownID（不 panic）；
//   - Await 读取已交付决策（resolved）或阻塞等待待决决策（pending）。
//
// 不实现 Immediate——Request 后驱动总是挂起，决策经 Resolve → Handle.Resume 注入续跑。
type InMemApproval struct {
	mu       sync.Mutex
	pending  map[ApprovalID]chan ApprovalDecision
	resolved map[ApprovalID]ApprovalDecision
}

// NewInMemApproval 创建空 Approval。
func NewInMemApproval() *InMemApproval {
	return &InMemApproval{
		pending:  make(map[ApprovalID]chan ApprovalDecision),
		resolved: make(map[ApprovalID]ApprovalDecision),
	}
}

// Request 发起审批：分配 ID 并预占 chan。驱动拿到 ID 后抛 SuspendError 退出 Run。
// 不阻塞；chan 容量 1，Resolve 时写入。
func (a *InMemApproval) Request(ctx context.Context, req ApprovalRequest) (ApprovalID, error) {
	id := ApprovalID(uuid.NewString())
	a.mu.Lock()
	a.pending[id] = make(chan ApprovalDecision, 1)
	a.mu.Unlock()
	return id, nil
}

// Resolve 回填决策：交付到 chan（通知等待者）并存入 resolved（供后续 Await）。
// 同一 ID 二次 Resolve 返 ErrAlreadyResolved；未知 ID 返 ErrUnknownID。
func (a *InMemApproval) Resolve(ctx context.Context, id ApprovalID, d ApprovalDecision) error {
	a.mu.Lock()
	if _, done := a.resolved[id]; done {
		a.mu.Unlock()
		return ErrAlreadyResolved
	}
	ch, ok := a.pending[id]
	if !ok {
		a.mu.Unlock()
		return ErrUnknownID
	}
	delete(a.pending, id)
	a.resolved[id] = d
	a.mu.Unlock()

	ch <- d
	close(ch)
	return nil
}

// Await 读取指定 ID 的决策：已 Resolve 立即返回；未 Resolve 阻塞等待（ctx 可取消）。
// 测试 / 本地平台注入决策用。
func (a *InMemApproval) Await(ctx context.Context, id ApprovalID) (ApprovalDecision, error) {
	a.mu.Lock()
	if d, done := a.resolved[id]; done {
		a.mu.Unlock()
		return d, nil
	}
	ch, ok := a.pending[id]
	a.mu.Unlock()
	if !ok {
		return ApprovalDecision{}, ErrUnknownID
	}
	select {
	case d, ok := <-ch:
		if !ok {
			return ApprovalDecision{}, ErrUnknownID
		}
		return d, nil
	case <-ctx.Done():
		return ApprovalDecision{}, ctx.Err()
	}
}

// Pending 返回当前未决审批数量（仅观测用，单测断言）。
func (a *InMemApproval) Pending() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}
