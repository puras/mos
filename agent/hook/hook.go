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
// 守卫与钩子的调用点由执行通道（agent/execpath）编排：裁决在先、钩子在后。
//
// **载荷冻结**（S5e）：工具调用的载荷（工具名 / 参数 / CallID）对钩子**不可变**——
// 钩子改写即违约，该调用被拒并说明原因。于是守卫判定的是这次调用的**最终**
// 载荷（载荷不可变 ⇒ 判定恒有效），守卫只跑一次，不会重复触发有副作用的守卫。
// 要修正入参请在工具内部做（工具自己 normalize）——这是 PI 与 DSH 独立收敛的
// 共识。守卫自身没有绕行出口——它不持有钩子总线，二者互不引用，这是结构性保证。
//
// 零开销降级（不变式 6）：两条总线都可为 nil，nil 上的注册与调用都是 no-op，
// 行为与未注入完全一致——不装扩展点的用户不付任何代价。
//
// 目录预算制：HookKind 是有预算的枚举，新增一种前先删一种。归档实现里的
// `request`（预留、无消费者）与 `turn-stopping`（S6 未用到终止钩子）均已删——
// 没有消费者的环节不是扩展点，是债务；首个消费者出现时再加回。
//
// 三个 tool/* 环节的职责（S5e 接线）：
//   - tool/pre-execute  裁决一次调用（允许/拒绝/请求确认/终止本轮），不改载荷；
//   - tool/execute      环绕真实派发（计时/重试/指标），运行在并发区，必须可重入；
//   - tool/post-execute 改写结果文本（裁剪/脱敏），可加重错误，不可洗白错误。
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
	// HookPreStep 改写模型输入（RAG / 注入 / 预算监控）——S6 接线：驱动在
	// 每次决策前（压缩检查之后）跑一遍 pre-step 瀑布。
	HookPreStep HookKind = "pre-step"
	// HookToolPreExecute 裁决一次工具调用（允许 / 拒绝 / 请求审批 / 终止本轮）——S5e 消费。
	// 载荷（工具名 / 参数 / CallID）对钩子**不可变**：钩子只做决策，不改调用。
	HookToolPreExecute HookKind = "tool/pre-execute"
	// HookToolExecute 环绕派发（超时 / 重试 / 指标 / 审计）——S5e 消费。
	// 钩子不调用 next 即短路（工具不执行）；并发区内运行，必须可重入。
	HookToolExecute HookKind = "tool/execute"
	// HookToolPostExecute 改写结果文本（裁剪 / 脱敏）——S5e 消费。
	// 可把成功改成失败（发现问题就报错），不可把失败洗成成功。
	HookToolPostExecute HookKind = "tool/post-execute"
)

// DecisionKind 是 pre-execute 环节的裁决种类（对齐 DSH 的 PreToolDecision）。
type DecisionKind string

const (
	// DecisionAllow 放行（零值之外的显式放行；未表态 = 放行）。
	DecisionAllow DecisionKind = "allow"
	// DecisionDeny 拒绝本次调用：工具不执行，理由回填给模型。
	DecisionDeny DecisionKind = "deny"
	// DecisionAsk 请求人工确认。当前未装配审批通道时按 fail-closed 拒绝
	// （对齐 DSH：没有 ApprovalService 时 ask 降级为 deny）。
	DecisionAsk DecisionKind = "ask"
	// DecisionCancel 终止本轮：本次调用拒绝，同批后续调用全部跳过。
	DecisionCancel DecisionKind = "cancel"
)

// Decision 是一次 pre-execute 裁决。
type Decision struct {
	// Kind 是裁决种类；零值（空串）表示「未表态」= 放行。
	Kind DecisionKind
	// Reason 是拒绝/审批/终止的理由（回填给模型，要写清楚原因）。
	Reason string
}

// Denied 报告该决策是否为拒绝类（deny / ask / cancel）。
func (d Decision) Denied() bool {
	switch d.Kind {
	case DecisionDeny, DecisionAsk, DecisionCancel:
		return true
	default:
		return false
	}
}

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
	// Decision 是 tool/pre-execute 的裁决（其他环节不取用）。
	// 钩子不得改写 ToolCall：载荷冻结，裁决只影响「放不放行」。
	Decision Decision
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
	return b.WaterfallTo(ctx, ev, nil)
}

// WaterfallTo 沿链执行，链尾动作为 terminal（可为 nil = 不做事）。
//
// terminal 是「环绕派发」的落点：tool/execute 环节把真正的工具调用放在这里，
// 钩子可在调用前后做事（计时 / 重试 / 指标），不调用 next 即短路（不执行）。
//
// pre-execute 环节的短路规则：链上第一个**非 allow** 决策生效，后续钩子不再
// 执行（决策不可被下游洗白）。未表态（Kind 为空）视为 allow。
func (b *HookBus) WaterfallTo(ctx context.Context, ev *HookEvent, terminal func(ctx context.Context, ev *HookEvent) (*HookEvent, error)) (*HookEvent, error) {
	if b == nil {
		if terminal == nil {
			return ev, nil
		}
		return terminal(ctx, ev)
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
		// 已有拒绝类决策：不再往下走（第一个非 allow 决策生效）。
		if cur.Decision.Denied() {
			return cur, nil
		}
		if i >= len(chain) {
			if terminal == nil {
				return cur, nil
			}
			return terminal(ctx, cur)
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
