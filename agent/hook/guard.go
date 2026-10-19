package hook

import (
	"context"
	"sync"
)

// Guard 是单调守卫：串行裁决，短路即拒；不可被 hook 改写绕过
// （Policy / 权限 / 身份校验是不可重排、不可绕过的安全防线）。
type Guard func(ctx context.Context, ev *HookEvent) error

// GuardBus 是守卫链：按注册顺序串行裁决，Add 在链尾，任一拒绝即整体拒绝。
//
// 单调性的两个来源：
//   - 顺序：守卫按注册次序执行，不因载荷被改写而重排（载荷不可被钩子改动，
//     因为裁决发生在钩子之前——编排点见 agent/execpath）；
//   - 短路：首个非 nil 错误立即终止链，后续守卫不再执行（不做「多数表决」）。
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

// Add 追加守卫到链尾，返回移除函数（幂等）。总线为 nil 或守卫为 nil 时返回 no-op。
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

// Allow 串行裁决：任一守卫返回非 nil 即整体拒绝，并立即停止后续守卫。
// 总线为 nil 或无守卫时一律放行（零开销降级：未装守卫 = 无安全约束，
// 但未注册工具的 fail-closed 兜底在 tool.Registry.Call，与此无关）。
func (b *GuardBus) Allow(ctx context.Context, ev *HookEvent) error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	entries := append([]guardEntry(nil), b.guards...)
	b.mu.RUnlock()

	for _, e := range entries {
		if err := e.g(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}
