// Package scope 提供 per-agent/per-session 注册作用域（架构 v2 §3.7）：
// 注册到作用域的钩子/工具/覆盖在生命周期结束时按注册逆序自动回滚
// （对齐 Cordis "registrations are reversible effects"）。
package scope

import "sync"

// Scope 是按注册逆序回滚的回收容器。典型用法：Agent 生命周期（AgentRegistry）
// 创建时注册资源，Dispose 时调用 Close 统一回滚；注册项也可单独撤销（On 返回值）。
type Scope struct {
	mu        sync.Mutex
	disposers []func()
	closed    bool
}

// New 创建空作用域。
func New() *Scope {
	return &Scope{}
}

// On 注册一个回收器（dispose 回调），返回可单独撤销的 disposer。
// 撤销（返回的 func）只移除该注册项，不影响其他注册；Close 时按注册逆序执行。
func (s *Scope) On(dispose func()) func() {
	if s == nil || dispose == nil {
		return func() {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// 作用域已关闭：立即执行并返回 no-op 撤销器。
		dispose()
		return func() {}
	}
	s.disposers = append(s.disposers, dispose)
	i := len(s.disposers) - 1
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if i >= len(s.disposers) || s.disposers[i] == nil {
				return
			}
			s.disposers[i] = nil
		})
	}
}

// Close 按注册逆序回滚全部回收器，幂等（重复调用无副作用）。
func (s *Scope) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	disposers := s.disposers
	s.disposers = nil
	s.mu.Unlock()

	for i := len(disposers) - 1; i >= 0; i-- {
		if disposers[i] != nil {
			disposers[i]()
		}
	}
}
