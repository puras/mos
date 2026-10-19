// Package execpath 提供工具调用的统一执行通道：全库唯一的「一批工具调用如何被
// 裁决、如何被调度执行、结果如何回填」的语义所在。语义中立于驱动——ReAct 与
// 将来的 PlanExecute / Scripted 共用同一通道，驱动只负责投影与收尾。
//
// 当前形态（S5a 骨架）刻意最小：preflight 空实现（恒通过）→ execute 串行
// → 结果按模型给定的调用顺序返回。后续切片只往里填，不改调用方：
//
//	S5b  preflight 接入 GuardBus：拒绝项以失败回填给模型，挂起项中止整批
//	S5c  Policy 规则层作为 Guard 的一环（规则命中即拒，fail-closed）
//	S5e  execute 改有界并发（声明 SequentialTool 的项在批内串行）；
//	      届时守卫判定先于 tool/call 日志，被拦截的调用是否留痕在这一步定
//
// 本包不落会话日志、不裁剪输出、不重试、不感知 turn 编号——日志语义属驱动
// （session），避免执行语义与日志语义纠缠。
package execpath

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
)

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
// 执行流程（S5a）：preflight 恒通过（无守卫）→ 按模型给定顺序逐个执行。
// 返回按**调用顺序**而非完成顺序排列——模型按序给出调用，回填就按序回，
// 调用方靠 CallID 对回每个结果，不必自己排序（并发化后此不变式尤其重要）。
//
// 错误分级：工具报错（含未注册工具名、参数解析失败）都转成 IsError 结果而非
// 返回 error——那是「本次调用失败」而非「系统故障」，回填给模型让它自我纠正。
// reg 为 nil 时全部判失败（fail-closed 兜底，不 panic）。
func ExecuteBatch(ctx context.Context, reg *tool.Registry, calls []llm.ToolCall) []Outcome {
	outcomes := make([]Outcome, 0, len(calls))
	for _, call := range calls {
		outcomes = append(outcomes, executeOne(ctx, reg, call))
	}
	return outcomes
}

// executeOne 执行单个调用并把返回值归一化成可回填文本。
func executeOne(ctx context.Context, reg *tool.Registry, call llm.ToolCall) Outcome {
	oc := Outcome{Call: call}
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
