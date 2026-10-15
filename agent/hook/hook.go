// Package hook 提供智能体可改写环节的瀑布钩子（HookBus）与单调守卫（GuardBus）。
package hook

import (
	"context"
	"sync"

	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
)

// HookKind 是可改写环节的枚举（见架构 v2 §3.4 / §4.3）。
type HookKind string

const (
	HookPreStep         HookKind = "pre-step"          // 改写模型输入（压缩/RAG/注入）
	HookRequest         HookKind = "request"           // 改写请求配置（预留，见 driver 接线说明）
	HookToolPreExecute  HookKind = "tool/pre-execute"  // 拒绝/改写工具调用（Policy/审批）
	HookToolExecute     HookKind = "tool/execute"      // around dispatch（超时/重试/指标）
	HookToolPostExecute HookKind = "tool/post-execute" // 改写结果/审计
	HookTurnStopping    HookKind = "turn-stopping"     // 终止判定（串行，无 next）
)

// HookEvent 是钩子链中流转的载荷：不同环节使用不同字段。
type HookEvent struct {
	Kind       HookKind
	Messages   []session.InternalMessage // pre-step：模型输入（可改写）
	Req        *llm.Request              // request：请求配置（可改写，预留）
	ToolCall   *llm.ToolCall             // tool/pre-execute、tool/execute
	Result     any                       // tool/execute、tool/post-execute：工具原始返回值
	ResultText string                    // tool/post-execute：给模型看的文本（截断后）
	Err        error                     // tool/execute：工具执行错误
	Turn       int
	Step       int
}

// Next 委托链下游；不调用则短路（拒绝/裁决）。
type Next func(ctx context.Context, ev *HookEvent) (*HookEvent, error)

// Hook 是瀑布钩子：收到 (payload, next)，可改写后委托下游。
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

// On 注册钩子，返回移除函数（幂等）。
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
func (b *HookBus) Waterfall(ctx context.Context, ev *HookEvent) (*HookEvent, error) {
	if b == nil {
		return ev, nil
	}
	b.mu.RLock()
	entries := append([]hookEntry(nil), b.hooks[ev.Kind]...)
	b.mu.RUnlock()
	chain := make([]Hook, len(entries))
	for i, e := range entries {
		chain[i] = e.h
	}

	var run func(i int, cur *HookEvent) (*HookEvent, error)
	run = func(i int, cur *HookEvent) (*HookEvent, error) {
		if i >= len(chain) {
			return cur, nil
		}
		h := chain[i]
		next := func(c context.Context, e *HookEvent) (*HookEvent, error) {
			return run(i+1, e)
		}
		return h(ctx, cur, next)
	}
	return run(0, ev)
}