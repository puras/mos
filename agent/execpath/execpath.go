// Package execpath 提供工具调用的统一执行通道：全库唯一的「一批工具调用如何被
// 裁决、如何被调度执行、结果如何回填」的语义所在。语义中立于驱动——ReAct 与
// 将来的 PlanExecute / Scripted 共用同一通道，驱动只负责投影与收尾。
//
// 形态（S5e）：三段
//
//	① preflight（串行，按模型调用序）
//	     守卫裁决（GuardBus，单调）→ pre-execute 钩子链（只决策，不改载荷）
//	② execute（屏障分组 + 有界滚动池）
//	     独占调用自成屏障；连续可并发调用成组，组内按上限并发；
//	     execute 钩子链环绕真实派发（并发区内，必须可重入）
//	③ 回填（串行，按模型调用序）
//	     post-execute 钩子链（裁剪 / 脱敏）→ 归一化为 Outcome
//
// 三条关键契约（对齐 DSH 的 tools 管道）：
//
//   - **载荷冻结**：钩子不得改写工具名 / 参数 / CallID。守卫判定的是这次调用
//     的**最终**载荷（载荷不可变 ⇒ 判定恒有效），且守卫只跑一次——不需要复检，
//     也不会重复触发有副作用的守卫（配额扣减、审批请求）。
//   - **只有执行体并发**：preflight 与回填都按模型序串行（守卫与钩子可能携带
//     顺序敏感状态），并发的只是工具体本身。
//   - **并发 fail-closed**：工具未声明可并发、参数非法、分类器出错 → 一律独占
//     （串行）。并发只影响完成顺序，**回填顺序永远是模型的调用序**。
//
// 本包不落会话日志、不裁剪输出、不重试——日志语义属驱动（session），
// 避免执行语义与日志语义纠缠。审批挂起（S5g）以 pre-execute 的「请求确认」
// 决策进入本管道，守卫链保持纯单调（不阻塞、不挂起）。
package execpath

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
)

// defaultConcurrency 是并发上限的缺省值。
//
// 注意：并发是**显式 opt-in** 的——工具必须声明本次调用可并发（见
// tool.ConcurrencySafe），否则一律独占。所以这个上限只影响「已声明可并发」
// 的那部分调用，默认行为（全部工具独占）仍是串行。
const defaultConcurrency = 4

// Options 是一次批量执行的可选能力。未填的项一律降级为不启用（零开销）。
type Options struct {
	// Turn 是所属轮次，原样转交守卫与钩子，供其做跨轮判定（如 S6 的重复调用检测）。
	Turn int
	// Guards 是单调守卫总线（可选，nil = 无安全约束，见 hook.GuardBus 的 nil 降级）。
	Guards *hook.GuardBus
	// Hooks 是可改写环节的瀑布钩子总线（可选，nil = 无扩展点）。
	Hooks *hook.HookBus
	// Concurrency 是并发上限（<=0 取默认 4；1 = 串行）。
	Concurrency int
}

// Outcome 是一次工具调用的执行结果。
type Outcome struct {
	// Call 是这次调用对应的原始请求（含 CallID 与模型给定的参数）。
	Call llm.ToolCall
	// Content 是回填给模型的文本：成功为工具返回值的 JSON，失败为失败原因。
	Content string
	// IsError 标记显式失败（工具报错、参数非法、工具未注册、被守卫/钩子拒绝）。
	// 协议消息无此语义，它是日志审计与模型自纠正的依据。
	IsError bool
	// Terminate 标记本次调用请求终止本轮（pre-execute 的「终止」决策）。
	// 同批后续调用会被跳过；驱动如何收尾由驱动层决定。
	Terminate bool
}

// plan 是一次调用的内部计划：preflight 定下的裁决与执行结果。
type plan struct {
	call    llm.ToolCall
	pending bool  // 需要执行
	raw     any   // 工具原始返回值
	err     error // 失败原因（被拒 / 工具报错 / 契约违约）
	term    bool  // 请求终止本轮
}

// ExecuteBatch 执行一批工具调用，返回与 calls 严格同序的结果。
//
// 执行流程：preflight 逐条裁决 → 按屏障分组执行（组内有界并发）→ 按序回填。
// 返回按**调用顺序**而非完成顺序排列——模型按序给出调用，回填就按序回，
// 调用方靠 CallID 对回每个结果，不必自己排序（并发化后此不变式尤其重要）。
//
// 错误分级（都进结果，不返回 error）：
//   - 守卫拒绝：以 IsError 回填拒绝理由，工具不执行；
//   - 扩展点拒绝 / 请求确认（无审批通道）/ 终止：以 IsError 回填；
//   - 载荷冻结违约（钩子试图改写调用）：以 IsError 回填；
//   - 工具报错（未注册工具名、参数非法、执行失败）：以 IsError 回填。
//
// 都是「本次调用失败」而非「系统故障」，回填给模型让它自我纠正；中断整条
// turn 的只有驱动侧的落盘失败与决策失败。裁决逐条独立：拒绝一条不影响同批
// 其他调用，模型能看出是哪一条被拒、为什么。
func ExecuteBatch(ctx context.Context, reg *tool.Registry, calls []llm.ToolCall, opts Options) []Outcome {
	if len(calls) == 0 {
		return nil
	}
	plans := preflight(ctx, reg, calls, opts)
	dispatch(ctx, reg, plans, opts)
	return finalize(ctx, plans, opts)
}

// preflight 是阶段①：逐条裁决（串行，按模型调用序）。
//
// 守卫先于钩子：守卫看到的是原始载荷；钩子随后只做决策，不得改写载荷
// （载荷冻结）。两者都看不到「被改写后的载荷」，因为不存在改写。
func preflight(ctx context.Context, reg *tool.Registry, calls []llm.ToolCall, opts Options) []*plan {
	plans := make([]*plan, len(calls))
	terminated := false
	for i, call := range calls {
		p := &plan{call: call}
		plans[i] = p

		if terminated {
			// 上一个调用请求终止本轮：本次调用跳过（记合成失败结果，
			// 保证 tool/call 与 tool/result 仍然配对）。
			p.err = errors.New("本轮已终止（扩展点请求），本次调用未执行")
			continue
		}

		// ① 守卫：单调裁决（nil 总线 = 零开销降级，直接放行）。
		if err := opts.Guards.Allow(ctx, &hook.HookEvent{
			Kind:     hook.HookToolPreExecute,
			ToolCall: call,
			Turn:     opts.Turn,
		}); err != nil {
			p.err = fmt.Errorf("工具调用被安全策略拒绝: %w", err)
			continue
		}

		// ② pre-execute 钩子链：只决策，不改载荷。
		ev, err := opts.Hooks.Waterfall(ctx, &hook.HookEvent{
			Kind:     hook.HookToolPreExecute,
			ToolCall: call,
			Turn:     opts.Turn,
		})
		if err != nil {
			p.err = fmt.Errorf("工具调用被扩展点拒绝: %w", err)
			continue
		}
		if ev == nil {
			p.err = errors.New("扩展点返回了空载荷，调用已拒绝")
			continue
		}
		// 载荷冻结：钩子改写工具名 / 参数 / CallID 即违约——拒绝执行并说明原因。
		if ev.ToolCall.Name != call.Name || ev.ToolCall.Args != call.Args || ev.ToolCall.ID != call.ID {
			p.err = errors.New("扩展点试图改写工具调用载荷（载荷冻结契约，调用已拒绝）")
			continue
		}
		switch ev.Decision.Kind {
		case hook.DecisionDeny:
			p.err = fmt.Errorf("工具调用被扩展点拒绝: %s", reasonOf(ev.Decision))
		case hook.DecisionAsk:
			// 没有审批通道时 fail-closed（对齐 DSH：未装配 ApprovalService 时
			// ask 降级为 deny）。S5g 落地审批后，这里改为挂起等待。
			p.err = fmt.Errorf("工具调用需要人工确认，但未装配审批通道（fail-closed）: %s", reasonOf(ev.Decision))
		case hook.DecisionCancel:
			p.err = fmt.Errorf("工具调用被终止: %s", reasonOf(ev.Decision))
			p.term = true
			terminated = true
		default:
			p.pending = true
		}
	}
	return plans
}

// dispatch 是阶段②：按屏障分组执行（组内有界并发）。
//
// 分组规则（对齐 DSH）：连续「可并发」的调用成组；**独占调用自成屏障**，
// 组与组之间顺序执行。例：[read(A), read(B), write(A), read(C)]
// → [read(A) ∥ read(B)] → [write(A)] → [read(C)]。
func dispatch(ctx context.Context, reg *tool.Registry, plans []*plan, opts Options) {
	limit := opts.Concurrency
	if limit <= 0 {
		limit = defaultConcurrency
	}
	for _, group := range planGroups(reg, plans) {
		runGroup(ctx, reg, plans, group, limit, opts)
	}
}

// planGroups 按模型序切分执行组：可并发的连续成组，独占调用自成屏障。
func planGroups(reg *tool.Registry, plans []*plan) [][]int {
	var groups [][]int
	var cur []int
	for i, p := range plans {
		if !p.pending {
			continue
		}
		if concurrencySafe(reg, p.call) {
			cur = append(cur, i)
			continue
		}
		if len(cur) > 0 {
			groups = append(groups, cur)
			cur = nil
		}
		groups = append(groups, []int{i}) // 独占 = 屏障
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// runGroup 执行一个组：滚动池最多 limit 个并发，组结束才进入下一组。
func runGroup(ctx context.Context, reg *tool.Registry, plans []*plan, group []int, limit int, opts Options) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for _, idx := range group {
		if ctx.Err() != nil {
			// 未派发：记合成失败结果，保证 tool/call 与 tool/result 配对不破。
			plans[idx].err = errors.New("调用在派发前被取消（上下文已结束）")
			continue
		}
		wg.Add(1)
		sem <- struct{}{} // 池满则等待一个完成（滚动池）
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			runOne(ctx, reg, plans[i], opts)
		}(idx)
	}
	wg.Wait()
}

// runOne 执行单个调用：execute 钩子链环绕真实派发。
//
// 钩子不调用 next 即短路（工具不执行）；钩子可以多次调用 next（重试）。
// 环绕环节同样受载荷冻结约束：改写载荷即违约。
func runOne(ctx context.Context, reg *tool.Registry, p *plan, opts Options) {
	if reg == nil {
		p.err = errors.New("工具注册表未装配")
		return
	}
	dispatchCall := func(ctx context.Context, ev *hook.HookEvent) (*hook.HookEvent, error) {
		out, err := reg.Call(ctx, ev.ToolCall.Name, json.RawMessage(ev.ToolCall.Args))
		ev.Result = out
		ev.Err = err
		return ev, nil
	}
	ev, err := opts.Hooks.WaterfallTo(ctx, &hook.HookEvent{
		Kind:     hook.HookToolExecute,
		ToolCall: p.call,
		Turn:     opts.Turn,
	}, dispatchCall)
	if err != nil {
		p.err = fmt.Errorf("工具执行被扩展点中断: %w", err)
		return
	}
	if ev == nil {
		p.err = errors.New("扩展点返回了空载荷，调用已拒绝")
		return
	}
	if ev.ToolCall.Name != p.call.Name || ev.ToolCall.Args != p.call.Args || ev.ToolCall.ID != p.call.ID {
		p.err = errors.New("扩展点试图改写工具调用载荷（载荷冻结契约，调用已拒绝）")
		return
	}
	p.raw = ev.Result
	p.err = ev.Err
}

// finalize 是阶段③：按模型序回填（串行）。
//
// post-execute 钩子链可改写回填文本；**错误只可加重不可减轻**（可把成功
// 改成失败，不能把失败洗成成功——错误是事实，审计日志不能被改写成成功）。
func finalize(ctx context.Context, plans []*plan, opts Options) []Outcome {
	outcomes := make([]Outcome, len(plans))
	for i, p := range plans {
		oc := Outcome{Call: p.call, Terminate: p.term}
		switch {
		case p.err != nil:
			oc.Content, oc.IsError = p.err.Error(), true
		default:
			b, merr := json.Marshal(p.raw)
			if merr != nil {
				// 返回值不可序列化：退化为 Go 文本，仍属正常结果
				oc.Content = fmt.Sprintf("%v", p.raw)
			} else {
				oc.Content = string(b)
			}
		}

		ev := &hook.HookEvent{
			Kind:       hook.HookToolPostExecute,
			ToolCall:   p.call,
			Result:     p.raw,
			ResultText: oc.Content,
			Err:        p.err,
			Turn:       opts.Turn,
		}
		out, herr := opts.Hooks.Waterfall(ctx, ev)
		if herr == nil && out != nil {
			if out.ResultText != oc.Content {
				oc.Content = out.ResultText
			}
			if !oc.IsError && out.Err != nil {
				// 加重错误：成功 → 失败
				oc.Content, oc.IsError = out.Err.Error(), true
			}
		}
		outcomes[i] = oc
	}
	return outcomes
}

// concurrencySafe 判定一次调用是否可与同批其他调用并发。
//
// fail-closed：工具未声明可并发、参数非法、分类器返回非 true、甚至 panic
// ——一律按独占（串行）处理。
func concurrencySafe(reg *tool.Registry, call llm.ToolCall) bool {
	if reg == nil {
		return false
	}
	tl, ok := reg.Get(call.Name)
	if !ok {
		return false
	}
	cs, ok := tl.(tool.ConcurrencySafe)
	if !ok {
		return false
	}
	return safeOrFalse(cs, json.RawMessage(call.Args))
}

// safeOrFalse 调用并发分类器，panic 一律按 false（独占）处理。
func safeOrFalse(cs tool.ConcurrencySafe, args json.RawMessage) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	return cs.ConcurrencySafe(args) == true
}

// reasonOf 取决策理由（缺省给一句可读的兜底）。
func reasonOf(d hook.Decision) string {
	if d.Reason == "" {
		return "（未提供理由）"
	}
	return d.Reason
}
