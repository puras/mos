package hook

import (
	"context"
	"sync"
)

// Guard 是单调守卫：串行裁决，短路即拒；不可被 hook 改写绕过
// （Policy / 权限 / 身份校验是不可重排、不可绕过的安全防线）。
type Guard func(ctx context.Context, ev *HookEvent) error

// GuardBus 是守卫链：Add 追加在链尾，Allow 任一拒绝即拒绝。
type GuardBus struct {
	mu     sync.RWMutex
	guards []guardEntry
	nextID uint64
}

type guardEntry struct {
	id uint64
	g  Guard
}

// NewGuardBus 创建空守卫链。
func NewGuardBus() *GuardBus {
	return &GuardBus{}
}

// Add 追加守卫到链尾，返回移除函数（幂等）。
func (b *GuardBus) Add(g Guard) func() {
	if b == nil || g == nil {
		return func() {}
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.guards = append(b.guards, guardEntry{id: id, g: g})
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			for i, e := range b.guards {
				if e.id == id {
					b.guards = append(b.guards[:i], b.guards[i+1:]...)
					return
				}
			}
		})
	}
}

// Allow 串行执行守卫：任一拒绝（返回非 nil error）即整体拒绝。
func (b *GuardBus) Allow(ctx context.Context, ev *HookEvent) error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	entries := append([]guardEntry(nil), b.guards...)
	b.mu.RUnlock()
	for _, e := range entries {
		g := e.g
		if err := g(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}