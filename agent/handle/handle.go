// Package handle 提供 Agent 生命周期句柄（架构 v2 §3.8，对齐 dsh AgentHandle）：
// create / resume / dispose。Registry 管理会话 ID → AgentHandle 映射，
// Resume 从持久化重放会话 + checkpoint 恢复挂起轮（M2.8 异步审批续跑）。
package handle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/checkpoint"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/scope"
	"hegq.net/mos/agent/session"
)

// NewAgent 是装配回调：为指定会话构建绑定的智能体实例与其 RunContext。
// Registry 依赖它完成会话级装配（由 assistant 提供，复用其驱动工厂）。
type NewAgent func(sess *session.Session) (*driver.Agent, *driver.RunContext, error)

// AgentHandle 是单个 Agent 的生命周期句柄（架构 v2 §3.8）：
//   - Create 后可用 Run / RunStream 执行；
//   - 挂起（SuspendError）后经 Resume(decision) 从 checkpoint 续跑；
//   - Dispose 释放（Scope 逆序回滚 + checkpoint 清理）。
type AgentHandle struct {
	// ID 是会话 ID（= Session.ID，句柄与会话一一对应）。
	ID      string
	Session *session.Session
	Agent   *driver.Agent
	RT      *driver.RunContext
	Scope   *scope.Scope
	Store   checkpoint.Store

	disposed bool
}

// Run 在句柄会话上执行一次任务（非流式）。
func (h *AgentHandle) Run(ctx context.Context, input string) (string, error) {
	if h == nil || h.Agent == nil {
		return "", fmt.Errorf("agent: handle: nil agent")
	}
	return h.Agent.Run(ctx, h.Session, input)
}

// Resume 从挂起点续跑（M2.8，架构 v2 §3.8/§10.6 PI SuspendedOperation）：
//   - 有 checkpoint：加载 → 注入决策表（Decisions[挂起 call ID] = decision）→ 重存 →
//     从挂起轮直接进入工具批量执行（跳过 LLM 调用与日志）；
//   - 无 checkpoint：普通续跑（新轮，崩溃恢复已由 RepairInterrupted 平衡日志）。
//
// 续跑期间若再次挂起，返回 *approval.SuspendError（调用方继续人工介入）。
// 任务正常完成时清理 checkpoint 与持久化任务状态。
func (h *AgentHandle) Resume(ctx context.Context, d approval.ApprovalDecision) (string, error) {
	if h == nil || h.Agent == nil {
		return "", fmt.Errorf("agent: handle: nil agent")
	}
	ckpt, err := h.Store.Load(ctx, h.Session.ID)
	if err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
		return "", fmt.Errorf("agent: handle resume: load checkpoint: %w", err)
	}
	if ckpt != nil {
		// 注入决策：approval guard 按挂起 call ID 查表放行（放行 / 拒绝）。
		ckpt.Decisions = map[string]approval.ApprovalDecision{ckpt.SuspendedCallID: d}
		now := time.Now()
		ckpt.ResolvedAt = &now
		if err := h.Store.Save(ctx, ckpt); err != nil {
			return "", fmt.Errorf("agent: handle resume: save checkpoint: %w", err)
		}
		h.RT.ResumeCheckpoint = ckpt
		_ = h.Session.Append(event.SessionEventApprovalResolve, event.ApprovalResolveData{
			ID:        string(ckpt.ApprovalID),
			Approved:  d.Approved,
			Reason:    d.Reason,
			DecidedBy: d.DecidedBy,
		})
	}
	reply, err := h.Agent.Run(ctx, h.Session, "")
	if err != nil {
		return "", err
	}
	// 任务完成：清理挂起点（checkpoint + 任务状态标记完成）。
	if h.Store != nil {
		_ = h.Store.Delete(ctx, h.Session.ID)
	}
	return reply, nil
}

// Dispose 释放句柄：Scope 逆序回滚 + 广播 agent/disposed（幂等）。
func (h *AgentHandle) Dispose(ctx context.Context) error {
	if h == nil || h.disposed {
		return nil
	}
	h.disposed = true
	if h.Scope != nil {
		h.Scope.Close()
	}
	h.RT.Notify(agent.Event{Type: agent.EventDisposed, SessionID: h.ID})
	_ = h.Session.Append(event.SessionEventAgentDisposed, event.AgentHandleData{
		HandleID: h.ID, SessionID: h.ID, Status: "disposed",
	})
	return nil
}

// Registry 是会话 ID → AgentHandle 的注册表（架构 v2 §3.8，对齐 dsh AgentRegistry）：
// Create 新建会话与句柄；Resume 从持久化恢复会话与句柄；DisposeAll 释放全部。
// 并发安全（细粒度乐观并发由业务层负责，本层以粗粒度锁保证一致性）。
type Registry struct {
	mu       sync.Mutex
	handles  map[string]*AgentHandle
	newAgent NewAgent
	store    checkpoint.Store
	bus      *event.EventBus // 可选：Agent 域事件广播（agent/created 等）
}

// NewRegistry 创建注册表。
func NewRegistry(newAgent NewAgent, store checkpoint.Store, bus *event.EventBus) *Registry {
	return &Registry{
		handles:  make(map[string]*AgentHandle),
		newAgent: newAgent,
		store:    store,
		bus:      bus,
	}
}

// Create 创建新会话与句柄。
func (r *Registry) Create(ctx context.Context, sess *session.Session) (*AgentHandle, error) {
	ag, rt, err := r.newAgent(sess)
	if err != nil {
		return nil, err
	}
	h := &AgentHandle{
		ID:      sess.ID,
		Session: sess,
		Agent:   ag,
		RT:      rt,
		Scope:   scope.New(),
		Store:   r.store,
	}
	r.mu.Lock()
	r.handles[sess.ID] = h
	r.mu.Unlock()
	r.emit(ctx, event.SessionEventAgentCreated, event.AgentHandleData{HandleID: sess.ID, SessionID: sess.ID, Status: "created"})
	return h, nil
}

// Resume 从持久化恢复会话并返回句柄（M2.8）：会话重放 + 资源校验 + checkpoint 挂载。
func (r *Registry) Resume(ctx context.Context, sess *session.Session) (*AgentHandle, error) {
	// 加载 checkpoint（若有）：刻意挂起标记。存在则挂到 RunContext 供续跑，
	// 并移除 RepairInterrupted 为挂起轮合成的 turn/end{interrupted}（续跑时再写）。
	ckpt, err := r.store.Load(ctx, sess.ID)
	if err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
		return nil, fmt.Errorf("agent: registry resume: load checkpoint: %w", err)
	}
	if ckpt != nil {
		session.DropInterruptedForTurn(sess, ckpt.Turn)
	}

	ag, rt, err := r.newAgent(sess)
	if err != nil {
		return nil, err
	}
	// 资源校验：会话事件引用的工具在当前 RunContext 中必须存在（PI SuspendedOperation missing 语义）。
	if err := ValidateMissing(rt, sess); err != nil {
		return nil, err
	}
	if ckpt != nil {
		rt.ResumeCheckpoint = ckpt
	}
	h := &AgentHandle{
		ID:      sess.ID,
		Session: sess,
		Agent:   ag,
		RT:      rt,
		Scope:   scope.New(),
		Store:   r.store,
	}
	r.mu.Lock()
	r.handles[sess.ID] = h
	r.mu.Unlock()
	r.emit(ctx, event.SessionEventAgentStatus, event.AgentHandleData{HandleID: sess.ID, SessionID: sess.ID, Status: "resumed"})
	return h, nil
}

// Get 按会话 ID 查询句柄。
func (r *Registry) Get(id string) (*AgentHandle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.handles[id]
	return h, ok
}

// DisposeAll 释放全部句柄（应用退出清理）。
func (r *Registry) DisposeAll(ctx context.Context) error {
	r.mu.Lock()
	hs := make([]*AgentHandle, 0, len(r.handles))
	for _, h := range r.handles {
		hs = append(hs, h)
	}
	r.mu.Unlock()
	var firstErr error
	for _, h := range hs {
		if err := h.Dispose(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// emit 广播 Agent 域事件（bus 可选）。
func (r *Registry) emit(ctx context.Context, name string, payload any) {
	if r.bus == nil {
		return
	}
	_ = r.bus.Emit(ctx, name, payload)
}
