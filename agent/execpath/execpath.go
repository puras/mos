// Package execpath 提供单波/单批工具调用的统一安全执行通道（架构 v2 §3.9）：
// react 批量与 scripted 编排各波共用，保证 GuardBus 决策 / 审批挂起 /
// tool/call 日志 / 钩子链 / 截断 / 审计语义全库唯一。语义中立于驱动：
// 不感知 ReAct 或 Scripted 编排，由调用方负责 ApplyOutcomes 落日志收尾。
package execpath

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/executor"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/agent/telemetry"
	"github.com/puras/mos/httpx"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
	fs "github.com/puras/mos/filesystem"
)

// ErrToolDenied 表示工具调用被单调守卫拒绝（非致命：结果反馈给模型继续）。
var ErrToolDenied = errors.New("agent: tool denied")

// ToolDeniedError 是守卫拒绝的携带原因错误。
type ToolDeniedError struct{ Reason string }

func (e *ToolDeniedError) Error() string { return "工具调用被安全策略拒绝: " + e.Reason }

// ToolOutcome 是单条工具调用的执行结果（result 为 execute 链原始返回值，
// text 为 post-execute 截断后的模型可见文本，err 为运行期错误）。
type ToolOutcome struct {
	Result any
	Text   string
	Err    error
}

// ExecuteBatch 批量执行工具调用（架构 v2 §3.9 三阶段，对齐 PI executeToolCallsParallel）：
//   - preflight（串行）：单调守卫决策（**先于** tool/call 日志——挂起时不落日志），
//     拒绝 = 拦截集合（显式失败回填）；挂起（*approval.SuspendError）= 整批中止（返回第二返回值）；
//   - execute（有界并发）：pre-execute → execute → post-execute 钩子链；任一工具声明
//     SequentialTool 时该项在批内串行执行（对齐 PI per-tool executionMode）；
//   - finalize（调用方）：结果按模型调用顺序回填（本函数只产出按源序的 Outcome）。
//
// 守卫拒绝返回 *ToolDeniedError（非致命，结果反馈给模型）；DoomLoop 致命错误原样返回
// （调用方终止任务）；其余错误视为运行期失败，进各自 Outcome。
// 返回第二个值 *approval.SuspendError：非 nil 表示有审批待决，整批已中止、未执行任何工具
// （已通过守卫并落日志的先行工具可能留下孤儿 tool/call，不影响模型面投影）。
func ExecuteBatch(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn, step int, calls []llm.ToolCall) ([]executor.Outcome[*llm.ToolCall, ToolOutcome], *approval.SuspendError) {
	items := make([]executor.Item[*llm.ToolCall], len(calls))
	for i := range calls {
		items[i] = executor.Item[*llm.ToolCall]{Input: &calls[i]}
		if t, ok := rt.Tools.Get(calls[i].Name); ok {
			if st, isSeq := t.(tool.SequentialTool); isSeq && st.Sequential() {
				items[i].Sequential = true
			}
		}
	}

	// 挂起信号经闭包捕获 + 取消传播：批内任一工具触发审批即中止整批。
	var batchSuspend *approval.SuspendError
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()

	preflight := func(ctx context.Context, call *llm.ToolCall) (bool, error) {
		ev := &hook.HookEvent{Kind: hook.HookToolPreExecute, ToolCall: call, Turn: turn, Step: step}
		if err := rt.Guards.Allow(ctx, ev); err != nil {
			var s *approval.SuspendError
			if errors.As(err, &s) {
				batchSuspend = s
				cancel() // 中止后续 preflight / execute
				return false, err
			}
			var doomErr *hook.DoomLoopError
			if errors.As(err, &doomErr) {
				return false, err // 致命守卫错误：循环层终止任务（状态 blocked）
			}
			return false, &ToolDeniedError{Reason: err.Error()}
		}
		// 守卫放行后才落日志 + 观测（挂起/拒绝的工具不产生 tool/call 日志）。
		if err := sess.AppendToolCall(turn, call); err != nil {
			return false, err // 日志写失败：终止任务（非拦截回填）
		}
		rt.Notify(agent.Event{Type: agent.EventToolCall, SessionID: sess.ID, Turn: turn, ToolCall: call})
		return true, nil
	}
	execute := func(ctx context.Context, call *llm.ToolCall) (ToolOutcome, error) {
		// 注入工具执行上下文（request_human_input 等人工介入工具读取）：
		// 当前调用 ID + 审批通道 + resume 决策表。
		ctx = tool.WithCallID(ctx, call.ID)
		if rt.Approval != nil {
			ctx = approval.WithApproval(ctx, rt.Approval)
		}
		if rt.ResumeCheckpoint != nil {
			ctx = approval.WithResumeDecisions(ctx, rt.ResumeCheckpoint.Decisions)
		}
		var out ToolOutcome
		err := telemetry.WithSpan(ctx, rt.Telemetry, telemetry.SpanToolExec,
			map[string]string{"tool": call.Name, "call_id": call.ID, "turn": strconv.Itoa(turn)},
			func(ctx context.Context) error {
				var err error
				out, err = executeTool(ctx, rt, call, turn, step)
				return err
			})
		return out, err
	}
	outs := executor.Run(bctx, items, rt.MaxToolParallel, preflight, execute)
	return outs, batchSuspend
}

// executeTool 执行单条工具调用的钩子链：tool/pre-execute（可改写/拒绝）→
// tool/execute（链尾调 Tools.Call）→ tool/post-execute（截断/审计）。
func executeTool(ctx context.Context, rt *driver.RunContext, call *llm.ToolCall, turn, step int) (ToolOutcome, error) {
	ev := &hook.HookEvent{Kind: hook.HookToolPreExecute, ToolCall: call, Turn: turn, Step: step}
	ev, err := rt.Hooks.Waterfall(ctx, ev) // pre-execute（可改写/拒绝）
	if err != nil {
		return ToolOutcome{Err: err}, nil
	}
	execEv := &hook.HookEvent{Kind: hook.HookToolExecute, ToolCall: ev.ToolCall, Turn: turn, Step: step}
	execEv, err = rt.Hooks.Waterfall(ctx, execEv)
	if err != nil {
		return ToolOutcome{Err: err}, nil
	}
	postEv := &hook.HookEvent{Kind: hook.HookToolPostExecute, ToolCall: ev.ToolCall, Result: execEv.Result, Err: execEv.Err, Turn: turn, Step: step}
	postEv, err = rt.Hooks.Waterfall(ctx, postEv)
	if err != nil {
		return ToolOutcome{Err: err}, nil
	}
	return ToolOutcome{Result: execEv.Result, Text: postEv.ResultText, Err: postEv.Err}, nil
}

// ApplyOutcomes 将一批 Outcome 落日志（tool/result + 观测），denied 用 ToolDeniedError
// 显式失败回填。返回 error：preflight 阶段的非守卫错误（如 tool/call 落日志失败）
// → 终止任务。挂起（*SuspendError）与致命（*hook.DoomLoopError）由调用方在调用
// 本函数前检测处理。
func ApplyOutcomes(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, calls []llm.ToolCall, outs []executor.Outcome[*llm.ToolCall, ToolOutcome]) error {
	for _, out := range outs {
		call := out.Input
		if out.Denied {
			var deniedErr *ToolDeniedError
			if errors.As(out.Err, &deniedErr) {
				_ = sess.AppendToolResult(turn, call, nil, "", deniedErr)
				rt.Notify(agent.Event{Type: agent.EventToolResult, SessionID: sess.ID, Turn: turn, ToolCall: call})
				continue
			}
			return fmt.Errorf("agent: tool preflight: %w", out.Err)
		}
		_ = sess.AppendToolResult(turn, call, out.Output.Result, out.Output.Text, out.Output.Err)
		rt.Notify(agent.Event{Type: agent.EventToolResult, SessionID: sess.ID, Turn: turn, ToolCall: call, ToolResult: out.Output.Result})
	}
	return nil
}

// ExecuteHook 是 tool/execute 链尾的默认执行器：实际调用工具并填充结果。
// 平台可在其前注册包装钩子（超时/重试/指标），经 next 委托到本执行器。
//
// 工具执行前向 ctx 注入四样能力：
//   - *session.Session（agent.WithSession）：activate_skill 等工具写事件 / 读本轮状态；
//   - rt.FS（fs.WithFileSystem）：内置文件工具（read_file / write_file / glob /
//     grep）经 fs.FromContext(ctx) 取用；未注入即 *fs.NotConfiguredError（fail-closed）。
//     注入点集中在此（三个驱动共用 execpath 通道），一处覆盖全部路径；
//   - rt.Bus（fs.WithBus）：Sandboxed.Resolve 的 fs/resolve 审计事件出口；
//   - rt.HTTPClient（httpx.WithClient）：联网工具（web_fetch / web_search）经
//     httpx.FromContext(ctx) 取用；nil 不注入，兜底 httpx.Default（带超时）。
func ExecuteHook(rt *driver.RunContext) hook.Hook {
	return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		if ev.ToolCall == nil {
			return next(ctx, ev)
		}
		if rt != nil && rt.Session != nil {
			ctx = agent.WithSession(ctx, rt.Session)
		}
		if rt != nil && rt.FS != nil {
			ctx = fs.WithFileSystem(ctx, rt.FS)
		}
		if rt != nil && rt.Bus != nil {
			ctx = fs.WithBus(ctx, rt.Bus)
		}
		if rt != nil && rt.HTTPClient != nil {
			ctx = httpx.WithClient(ctx, rt.HTTPClient)
		}
		result, err := rt.Tools.Call(ctx, ev.ToolCall.Name, json.RawMessage(ev.ToolCall.Args))
		ev.Result = result
		if err != nil {
			ev.Err = err
		}
		return next(ctx, ev)
	}
}

// TruncateHook 是 tool/post-execute 环节的上下文瘦身：对给模型看的工具结果文本
// 做行数/字节上限截断（超限携带逃生通道提示）；审计仍使用原始 Result。
func TruncateHook(rt *driver.RunContext) hook.Hook {
	return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		if ev.ToolCall == nil || rt.Truncator == nil {
			return next(ctx, ev)
		}
		text := session.FormatToolResult(ev.Result)
		if s, truncated := rt.Truncator.Truncate(ev.ToolCall.Name, text); truncated {
			ev.ResultText = s
		} else {
			ev.ResultText = text
		}
		return next(ctx, ev)
	}
}

// AuditHook 是 tool/post-execute 环节的审计门（Audit Gate）：
// 仅将审计器认可的环境事实写入任务状态（TaskState），并广播 EventFact。
func AuditHook(rt *driver.RunContext) hook.Hook {
	return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		if ev.ToolCall == nil || ev.Err != nil || rt.Auditor == nil {
			return next(ctx, ev)
		}
		facts, err := rt.Auditor.Audit(ctx, ev.ToolCall, ev.Result)
		if err != nil {
			return next(ctx, ev)
		}
		// 批量工具并发执行时多个 goroutine 同时写同一 Session.State.Facts，
		// 经 rt.AddFact 串行化（锁外广播 Fact 事件，避免 Observer 回调持锁）。
		for _, f := range facts {
			rt.AddFact(f)
			f := f
			rt.Notify(agent.Event{Type: agent.EventFact, SessionID: rt.Session.ID, Turn: ev.Turn, Fact: &f})
		}
		return next(ctx, ev)
	}
}
