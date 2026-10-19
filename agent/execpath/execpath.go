// Package execpath 提供工具调用的统一执行通道：全库唯一的「一批工具调用如何被
// 裁决、如何被调度执行、结果如何回填」的语义所在。语义中立于驱动——ReAct 与
// 将来的 PlanExecute / Scripted 共用同一通道，驱动只负责投影与收尾。
//
// 当前形态（S5b 已接守卫）：preflight 串行裁决（GuardBus）→ execute 按序串行
// → 结果按模型给定的调用顺序返回。后续切片只往里填，不改调用方：
//
//	S5e  execute 改有界并发（声明 SequentialTool 的项在批内串行）；
//	      preflight 后接 hook 的 tool/pre-execute 与 tool/execute 环节；
//	      届时审批挂起（S5g）以「中止整批」形式进入 preflight
//
// 本包不落会话日志、不裁剪输出、不重试——日志语义属驱动（session），
// 避免执行语义与日志语义纠缠。
package execpath

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
)

// Options 是一次批量执行的可选能力。未填的项一律降级为不启用（零开销）。
type Options struct {
	// Turn 是所属轮次，原样转交守卫，供其做跨轮判定（如 S6 的重复调用检测）。
	Turn int
	// Guards 是单调守卫总线（可选，nil = 无安全约束，见 hook.GuardBus 的 nil 降级）。
	Guards *hook.GuardBus
}

// Outcome 是一次工具调用的执行结果。
type Outcome struct {
	// Call 是这次调用对应的原始请求（含 CallID 与模型给定的参数）。
	Call llm.ToolCall
	// Content 是回填给模型的文本：成功为工具返回值的 JSON，失败为失败原因。
	Content string
	// IsError 标记显式失败（工具报错、参数非法、工具未注册）。
	// 协议消息无此语义，它是日志审计与模型自纠正的依据。
	IsError bool
}

// ExecuteBatch 执行一批工具调用，返回与 calls 严格同序的结果。
//
// 执行流程：preflight 守卫裁决 → 按模型给定顺序逐个执行 → 按序返回结果。
// 返回按**调用顺序**而非完成顺序排列——模型按序给出调用，回填就按序回，
// 调用方靠 CallID 对回每个结果，不必自己排序（并发化后此不变式尤其重要）。
//
// 错误分级（两类都进结果，不返回 error）：
//   - 守卫拒绝：以 IsError 回填拒绝理由，工具不执行；
//   - 工具报错（未注册工具名、参数非法、执行失败）：以 IsError 回填。
//
// 都是「本次调用失败」而非「系统故障」，回填给模型让它自我纠正；中断整条
// turn 的只有驱动侧的落盘失败与决策失败。守卫逐条独立裁决：拒绝一条不影响
// 同批其他调用，模型能看出是哪一条被拒、为什么。
func ExecuteBatch(ctx context.Context, reg *tool.Registry, calls []llm.ToolCall, opts Options) []Outcome {
	outcomes := make([]Outcome, 0, len(calls))
	for _, call := range calls {
		outcomes = append(outcomes, runOne(ctx, reg, opts, call))
	}
	return outcomes
}

// runOne 对单个调用先裁决后执行，并把返回值归一化成可回填文本。
// 守卫在先：它看到的是未经钩子改写的原始载荷（双轨制，见 agent/hook）。
// 未装守卫时 opts.Guards 为 nil，Allow 直接放行（零开销降级）。
func runOne(ctx context.Context, reg *tool.Registry, opts Options, call llm.ToolCall) Outcome {
	oc := Outcome{Call: call}

	if err := opts.Guards.Allow(ctx, &hook.HookEvent{
		Kind:     hook.HookToolPreExecute,
		ToolCall: call,
		Turn:     opts.Turn,
	}); err != nil {
		oc.Content, oc.IsError = fmt.Sprintf("工具调用被安全策略拒绝: %v", err), true
		return oc
	}

	if reg == nil {
		oc.Content, oc.IsError = "工具注册表未装配", true
		return oc
	}
	out, err := reg.Call(ctx, call.Name, json.RawMessage(call.Args))
	if err != nil {
		oc.Content, oc.IsError = err.Error(), true
		return oc
	}
	b, err := json.Marshal(out)
	if err != nil {
		// 返回值不可序列化：退化为 Go 文本，仍属正常结果
		oc.Content = fmt.Sprintf("%v", out)
		return oc
	}
	oc.Content = string(b)
	return oc
}
