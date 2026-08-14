package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// DispatchMode 是事件分发模式（对齐 dsh/Cordis 事件四模式）：
//   - ModeEmit：观察。按注册顺序调用，无返回值，用于观测/日志。
//   - ModeWaterfall：包装。按注册顺序调用，返回值向下传播，可短路（改写链）。
//   - ModeParallel：扇出。并发执行，无返回值，用于并行观测。
//   - ModeSerial：裁决。按注册顺序调用，任一返回 false 即短路（策略决策）。
type DispatchMode int

const (
	ModeEmit DispatchMode = iota
	ModeWaterfall
	ModeParallel
	ModeSerial
)

// EventHandler 是事件监听器。
// Emit/Parallel 模式下返回值被忽略；Waterfall 模式下返回值（any）作为下一监听器输入；
// Serial 模式下返回的 bool 为裁决结果，error 表示处理失败。
type EventHandler func(ctx context.Context, payload any) (any, error)

type eventListener struct {
	id uint64
	h  EventHandler
}

// EventBus 是事件分发器：name → 有序监听器列表。
// 事件是扩展点，模式是事件的公开契约（见架构 v2 §3.1）。
type EventBus struct {
	mu        sync.RWMutex
	listeners map[string][]eventListener
	nextID    uint64
}

// NewEventBus 创建空事件总线。
func NewEventBus() *EventBus {
	return &EventBus{listeners: make(map[string][]eventListener)}
}

// On 注册事件监听器，返回移除函数（幂等）。
func (b *EventBus) On(name string, mode DispatchMode, h EventHandler) func() {
	if b == nil || h == nil {
		return func() {}
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.listeners[name] = append(b.listeners[name], eventListener{id: id, h: h})
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			ls := b.listeners[name]
			for i, l := range ls {
				if l.id == id {
					b.listeners[name] = append(ls[:i], ls[i+1:]...)
					return
				}
			}
		})
	}
}

// Emit 观察分发：按注册顺序调用，忽略返回值；任一监听器出错即返回（后续不执行）。
func (b *EventBus) Emit(ctx context.Context, name string, payload any) error {
	ls := b.snapshot(name)
	for _, l := range ls {
		if _, err := l.h(ctx, payload); err != nil {
			return err
		}
	}
	return nil
}

// Waterfall 包装分发：payload 沿监听器链传播，返回最终 payload。
// 监听器返回 error 即中断；返回 (nil, nil) 表示短路（保持当前值返回）。
func (b *EventBus) Waterfall(ctx context.Context, name string, payload any) (any, error) {
	ls := b.snapshot(name)
	cur := payload
	for _, l := range ls {
		out, err := l.h(ctx, cur)
		if err != nil {
			return nil, err
		}
		if out == nil {
			return cur, nil // 短路
		}
		cur = out
	}
	return cur, nil
}

// Serial 裁决分发：按注册顺序调用；监听器返回值断言为 bool，
// 任一返回 false 即短路裁决失败。
func (b *EventBus) Serial(ctx context.Context, name string, payload any) (bool, error) {
	ls := b.snapshot(name)
	for _, l := range ls {
		out, err := l.h(ctx, payload)
		if err != nil {
			return false, err
		}
		ok, isBool := out.(bool)
		if !isBool {
			return false, fmt.Errorf("eventbus: %q serial listener must return bool", name)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// Parallel 扇出分发：并发调用全部监听器，聚合首个错误。
func (b *EventBus) Parallel(ctx context.Context, name string, payload any) error {
	ls := b.snapshot(name)
	if len(ls) == 0 {
		return nil
	}
	errs := make(chan error, len(ls))
	var wg sync.WaitGroup
	for _, l := range ls {
		wg.Add(1)
		go func(h EventHandler) {
			defer wg.Done()
			if _, err := h(ctx, payload); err != nil {
				errs <- err
			}
		}(l.h)
	}
	wg.Wait()
	close(errs)
	return errors.Join(collectErrors(errs)...)
}

func collectErrors(ch <-chan error) []error {
	var out []error
	for err := range ch {
		out = append(out, err)
	}
	return out
}

func (b *EventBus) snapshot(name string) []eventListener {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]eventListener(nil), b.listeners[name]...)
}
