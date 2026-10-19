// Package hook 提供智能体的双轨制扩展点：可改写环节的瀑布钩子（HookBus）与
// 安全约束的单调守卫（GuardBus）。
//
// 双轨制是不可混同的分工（架构不变式 5）：
//
//	HookBus   waterfall   可改写、可短路、可替换——行为性扩展（压缩 / RAG /
//	                      注入 / 重试 / 指标 / 结果裁剪），按注册顺序层层委托
//	GuardBus  serial      单调裁决、任一拒绝即拒绝——安全性约束（Policy /
//	                      权限 / 身份），不可重排、不可被钩子绕过
//
// 守卫与钩子的调用点由执行通道（agent/execpath）编排：裁决在先、钩子在后，
// 因此钩子改写工具调用参数也改变不了守卫的判定（守卫看到的是改写前的载荷）。
// 守卫自身没有绕行出口——它不持有钩子总线，二者互不引用，这是结构性保证。
//
// 零开销降级（不变式 6）：两条总线都可为 nil，nil 上的注册与调用都是 no-op，
// 行为与未注入完全一致——不装扩展点的用户不付任何代价。
//
// 目录预算制：HookKind 是有预算的枚举，新增一种前先删一种。归档实现里的
// `request`（预留、无消费者）已删；doomloop（重复调用检测）属 S6，届时随
// turn-stopping 一并落地。
package hook

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/puras/mos/llm"
)

// HookKind 是可改写环节的枚举。
type HookKind string

const (
	// HookPreStep 改写模型输入（压缩 / RAG / 注入）——S6 消费。
	HookPreStep HookKind = "pre-step"
	// HookToolPreExecute 拒绝或改写工具调用（工具入参修正、审批接入）——S5e 消费。
	HookToolPreExecute HookKind = "tool/pre-execute"
	// HookToolExecute 环绕派发（超时 / 重试 / 指标 / 审计）——S5e 消费。
	HookToolExecute HookKind = "tool/execute"
	// HookToolPostExecute 改写结果文本（裁剪 / 脱敏）——S5e 消费。
	HookToolPostExecute HookKind = "tool/post-execute"
	// HookTurnStopping 终止判定（串行，无 next）——S6 消费。
	HookTurnStopping HookKind = "turn-stopping"
)

// ErrNilHookEvent 表示钩子返回了 (nil, nil)：链上载荷凭空消失，
// 无法继续委托。显式报错而非把 nil 传下去崩在下游——坏插件不许带崩整条链。
var ErrNilHookEvent = errors.New("hook: 钩子返回了 nil 事件")

// HookEvent 是钩子链与守卫链中流转的载荷：不同环节使用不同字段。
type HookEvent struct {
	// Kind 是环节（决定取用哪些字段，见 HookKind 注释）。
	Kind HookKind
	// Messages 是 pre-step 的模型输入（可改写）。
	Messages []llm.Message
	// ToolCall 是 tool/* 环节的被调用工具（可改写：换参、换工具）。
	ToolCall llm.ToolCall
	// Result 是 tool/execute、tool/post-execute 的工具原始返回值。
	Result any
	// ResultText 是 post-execute 给模型看的文本（裁剪 / 脱敏后，可改写）。
	ResultText string
	// Err 是 tool/execute 的执行错误（post-execute 可据此改写回填文本）。
	Err error
	// Turn 是所属轮次，守卫与钩子据此做跨轮判定（如重复调用检测）。
	Turn int
}

// Next 委托链下游；不调用则短路（拒绝 / 提前定案）。
type Next func(ctx context.Context, ev *HookEvent) (*HookEvent, error)

// Hook 是瀑布钩子：收到 (载荷, next)，可改写后委托下游，也可不委托而短路。
type Hook func(ctx context.Context, ev *HookEvent, next Next) (*HookEvent, error)

// HookBus 是瀑布钩子总线：按注册顺序构建 waterfall 链。
// 钩子可改写载荷、短路（不调用 next）或替换实现（调用 next 前改写）。
type HookBus struct {
	mu     sync.RWMutex
	hooks  map[HookKind][]hookEntry
	nextID uint64
}

type hookEntry struct {
	id uint64
	h  Hook
}

// NewHookBus 创建空钩子总线。
func NewHookBus() *HookBus {
	return &HookBus{hooks: make(map[HookKind][]hookEntry)}
}

// On 注册钩子，返回移除函数（幂等）。总线为 nil 或钩子为 nil 时返回 no-op 移除函数。
func (b *HookBus) On(kind HookKind, h Hook) func() {
	if b == nil || h == nil {
		return func() {}
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.hooks[kind] = append(b.hooks[kind], hookEntry{id: id, h: h})
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			ls := b.hooks[kind]
			for i, e := range ls {
				if e.id == id {
					b.hooks[kind] = append(ls[:i], ls[i+1:]...)
					return
				}
			}
		})
	}
}

// Waterfall 沿链执行：每个钩子收到 next（委托下游），不调用则短路。
// 总线为 nil 时原样返回载荷（零开销降级）。
func (b *HookBus) Waterfall(ctx context.Context, ev *HookEvent) (*HookEvent, error) {
	if b == nil {
		return ev, nil
	}
	if ev == nil {
		return nil, ErrNilHookEvent
	}
	b.mu.RLock()
	entries := append([]hookEntry(nil), b.hooks[ev.Kind]...)
	b.mu.RUnlock()

	// 快照成链：注册与执行并发安全，且钩子内注册/移除同种钩子不影响本次执行
	chain := make([]Hook, len(entries))
	for i, e := range entries {
		chain[i] = e.h
	}

	var run func(i int, cur *HookEvent) (*HookEvent, error)
	run = func(i int, cur *HookEvent) (*HookEvent, error) {
		if i >= len(chain) {
			return cur, nil
		}
		next := func(c context.Context, e *HookEvent) (*HookEvent, error) {
			return run(i+1, e)
		}
		out, err := chain[i](ctx, cur, next)
		if err != nil {
			return out, err
		}
		if out == nil {
			return nil, fmt.Errorf("%w（%s）", ErrNilHookEvent, ev.Kind)
		}
		return out, nil
	}
	return run(0, ev)
}
