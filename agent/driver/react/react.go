// Package react 提供默认 ReAct 循环驱动（架构 v2 §3.3/§11.3）：
// ReActDriver 实现 LoopDriver，NewReAct 注册默认钩子/守卫，New 是默认装配入口。
package react

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/checkpoint"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/executor"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/agent/telemetry"
	"hegq.net/mos/llm"
	"hegq.net/mos/memory"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// ErrToolDenied 表示工具调用被单调守卫拒绝（非致命：结果反馈给模型继续）。
var ErrToolDenied = errors.New("agent: tool denied")

// ToolDeniedError 是守卫拒绝的携带原因错误。
type ToolDeniedError struct{ Reason string }

func (e *ToolDeniedError) Error() string { return "工具调用被安全策略拒绝: " + e.Reason }

// ReActDriver 是默认循环驱动：推理 → 工具调用 直至产出最终回复
// （现有 run 循环的搬移，行为不变；见架构 v2 §3.3）。
type ReActDriver struct{}

// NewReAct 返回默认装配的 ReAct 驱动，并注册默认钩子/守卫
// （行为与旧版默认配置等价，见架构 v2 §3.4）：
//   - pre-step：compressHook（两轮之间压缩）+ ragInjectHook（RAG 注入）
//   - tool/execute：executeHook（链尾实际执行，平台可在其前包装超时/重试）
//   - tool/post-execute：truncateHook（结果文本瘦身）+ auditHook（审计门）
//   - Guards：policyGuard（Policy 单调守卫，不可被改写绕过）
//
// 预留 HookRequest / HookToolPreExecute / HookTurnStopping 环节供平台扩展。
func NewReAct(rt *driver.RunContext) driver.LoopDriver {
	if rt.Hooks == nil {
		rt.Hooks = hook.NewHookBus()
	}
	if rt.Guards == nil {
		rt.Guards = hook.NewGuardBus()
	}
	if rt.Auditor == nil {
		rt.Auditor = agent.NewDefaultAuditor()
	}
	if rt.Truncator == nil {
		rt.Truncator = agent.NewDefaultTruncator(agent.TruncatorConfig{})
	}
	if rt.Compactor == nil {
		rt.Compactor = agent.NewDefaultCompactor(agent.CompactorConfig{})
	}

	rt.Hooks.On(hook.HookPreStep, compressHook(rt))
	rt.Hooks.On(hook.HookPreStep, ragInjectHook(rt))
	rt.Hooks.On(hook.HookToolExecute, executeHook(rt))
	rt.Hooks.On(hook.HookToolPostExecute, truncateHook(rt))
	rt.Hooks.On(hook.HookToolPostExecute, auditHook(rt))
	if rt.Policy != nil {
		rt.Guards.Add(policyGuard(rt))
	}
	// Doom Loop 检测以单调守卫注册（tool/pre-execute 阶段、Policy 之后）：
	// 相同工具连续相同参数调用达到阈值即中断任务（状态 blocked），见 agent/doomloop.go。
	if rt.DoomLoopMaxRepeat > 0 {
		rt.Guards.Add(hook.NewDoomLoopGuard(rt.DoomLoopMaxRepeat).Guard)
	}
	return &ReActDriver{}
}

// New 返回默认装配的 ReAct 智能体（架构 v2 §11.3 默认入口）：
// ReActDriver + 默认钩子/守卫 + RunContext，等价于旧版 agent.New(rt)。
func New(rt *driver.RunContext) *driver.Agent {
	return driver.New(rt, NewReAct(rt))
}

// Run 实现 LoopDriver。
func (d *ReActDriver) Run(ctx context.Context, rt *driver.RunContext, input string) (string, error) {
	if rt == nil || rt.Session == nil {
		return "", fmt.Errorf("agent: run requires session")
	}
	if rt.Tools == nil {
		return "", fmt.Errorf("agent: no tool registry configured")
	}
	if rt.Planner == nil {
		return "", fmt.Errorf("agent: no planner configured")
	}
	// 裸实例惰性默认装配：Router 等场景直接构造 ReActDriver 时，Run 前补全
	// 默认钩子/守卫（react.New 等显式装配入口调用后二者皆非 nil，此处跳过）。
	if rt.Hooks == nil || rt.Guards == nil {
		NewReAct(rt)
	}
	sess := rt.Session
	ctx = tool.WithSandbox(ctx, rt.Sandbox)

	// 任务状态初始化：首次运行记录原始目标，进入 running。
	// （resume 路径由 Handle.Resume 预加载持久化的 TaskState，Goal 已非空。）
	if sess.State.Goal == "" {
		sess.State.Goal = input
	}
	if sess.State.Status != session.StatusRunning {
		sess.State.Status = session.StatusRunning
	}

	// M2.8 resume：从挂起轮续跑，不追加 turn/start 与 user/message
	// （该轮事件已在挂起前落库；turn 计数器沿用挂起轮）。
	firstTurn := sess.NextTurn()
	if ckpt := rt.ResumeCheckpoint; ckpt != nil {
		firstTurn = ckpt.Turn
	} else {
		if err := sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: firstTurn, Reason: "user"}); err != nil {
			return "", fmt.Errorf("agent: append turn/start: %w", err)
		}
		if err := sess.AppendUserMessage(input, "user"); err != nil {
			return "", fmt.Errorf("agent: append user/message: %w", err)
		}
	}

	history := sess.Messages() // 不含 system（system 每轮注入，不写日志）

	for turn := firstTurn; ; turn++ {
		// 可选保险丝（默认关闭，对齐 DSH/PI 无轮次上限）：显式设置 >0 时兜底防失控。
		// 按本次 Run 内的轮次数计数（续跑会话 Turn 全局递增，上限语义不变）。
		if rt.MaxTurns > 0 && turn-firstTurn+1 > rt.MaxTurns {
			sess.State.Status = session.StatusBlocked
			_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: turn, Reason: "max_turns"})
			return "", fmt.Errorf("agent: max turns %d exceeded", rt.MaxTurns)
		}
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("agent: run canceled: %w", err)
		}
		reply, done, err := d.runTurn(ctx, rt, sess, turn, history)
		if err != nil {
			return "", err
		}
		if done {
			return reply, nil
		}
		history = sess.Messages() // 本轮工具执行后历史更新（直接作答/停止分支已 done）
	}
}

// runTurn 执行一轮循环并打 driver.turn span（架构 v2 §3.11）。
// 返回 (reply, done, err)：done=true 表示任务本轮收尾（直接作答 / 外部停止），
// reply 为最终回复；done=false 表示继续下一轮。
func (d *ReActDriver) runTurn(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, history []session.InternalMessage) (reply string, done bool, err error) {
	err = telemetry.WithSpan(ctx, rt.Telemetry, telemetry.SpanDriverTurn,
		map[string]string{"turn": strconv.Itoa(turn), "driver": "react"},
		func(ctx context.Context) error {
			reply, done, err = d.runTurnInner(ctx, rt, sess, turn, history)
			return err
		})
	return reply, done, err
}

// runTurnInner 是一轮循环的实现（pre-step → 规划 → 工具/作答 → 停止裁决）。
func (d *ReActDriver) runTurnInner(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, history []session.InternalMessage) (string, bool, error) {
	const step = 1

	// M2.8 resume：从挂起轮直接进入工具批量执行（跳过 LLM 调用与日志重放）。
	if ckpt := rt.ResumeCheckpoint; ckpt != nil && ckpt.Turn == turn {
		return d.runResumeTurn(ctx, rt, sess, turn, step, ckpt)
	}

	// 每轮刷新系统提示词：状态槽位随 TaskState 进度更新（M2.6 动态组装）
	sysText, err := rt.AssembleSystem(ctx)
	if err != nil {
		return "", false, fmt.Errorf("agent: assemble system: %w", err)
	}

	msgs := make([]session.InternalMessage, 0, len(history)+1)
	if sysText != "" {
		msgs = append(msgs, session.SystemMsg(sysText))
	}
	msgs = append(msgs, history...)

	// pre-step 钩子：压缩 + RAG 注入（可改写模型输入）
	hev := &hook.HookEvent{Kind: hook.HookPreStep, Messages: msgs, Turn: turn, Step: step}
	hev, err = rt.Hooks.Waterfall(ctx, hev)
	if err != nil {
		return "", false, fmt.Errorf("agent: pre-step hooks: %w", err)
	}

	if err := sess.Append(event.SessionEventStepStart, event.StepData{Turn: turn, Step: step}); err != nil {
		return "", false, fmt.Errorf("agent: append step/start: %w", err)
	}

	action, err := d.nextAction(ctx, rt, sess, turn, hev.Messages)
	if err != nil {
		return "", false, fmt.Errorf("agent: plan turn %d: %w", turn, err)
	}

	calls := action.ToolCalls
	if len(calls) == 0 && action.ToolCall != nil {
		calls = []llm.ToolCall{*action.ToolCall} // 兼容仅填 ToolCall 的第三方规划器
	}
	if err := sess.AppendAssistantMessage(turn, action.Content, action.ReasoningContent, calls, action.Usage); err != nil {
		return "", false, fmt.Errorf("agent: append assistant/message: %w", err)
	}
	rt.MonitorUsage(turn, action.Usage)
	if action.Content != "" {
		rt.Notify(agent.Event{Type: agent.EventStep, SessionID: sess.ID, Turn: turn, Content: action.Content})
	}

	if len(calls) == 0 {
		// 直接作答，任务完成
		sess.State.Status = session.StatusCompleted
		_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: turn, Step: step})
		_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: turn, Reason: "done"})
		rt.Notify(agent.Event{Type: agent.EventDone, SessionID: sess.ID, Turn: turn, Content: action.Content})
		return action.Content, true, nil
	}

	// 截断检测（对齐 PI failToolCallsFromTruncatedMessage）：
	// 输出被截断（finish_reason=length/max_tokens）时，已产出的工具调用可能不完整，
	// fail-closed：不执行、逐个显式失败回填（模型下一轮看到失败原因后调整），
	// 避免"已声明调用但模型面无回填"的静默丢弃。
	if isTruncatedFinish(action.FinishReason) {
		for i := range calls {
			call := &calls[i]
			err := fmt.Errorf("模型输出被截断（finish_reason=%s），工具调用未执行（fail-closed）", action.FinishReason)
			_ = sess.AppendToolResult(turn, call, nil, "", err)
			rt.Notify(agent.Event{Type: agent.EventToolResult, SessionID: sess.ID, Turn: turn, ToolCall: call})
		}
		_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: turn, Step: step})
		return "", false, nil
	}

	// 工具调用：批量执行（并发 + 按源序回填，架构 v2 §3.9 ConcurrentExecutor 三阶段）
	outs, suspend := execBatchTools(ctx, rt, sess, turn, step, calls)
	if suspend != nil {
		// M2.8 挂起：审批待决，写 checkpoint 后冒泡（不 append step/end / turn/end）。
		if err := d.saveCheckpoint(ctx, rt, sess, turn, step, calls, suspend); err != nil {
			return "", false, fmt.Errorf("agent: save checkpoint: %w", err)
		}
		return "", false, suspend
	}

	return d.finalizeToolBatch(ctx, rt, sess, turn, step, calls, &action, outs)
}

// runResumeTurn 是 M2.8 恢复路径：跳过 LLM 调用与日志，从 checkpoint 重新执行
// 挂起轮的工具批量（Calls），决策表（Decisions）经 rt.ResumeCheckpoint 注入
// approval guard 放行。二次挂起时重写 checkpoint 并冒泡。
func (d *ReActDriver) runResumeTurn(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn, step int, ckpt *checkpoint.Checkpoint) (string, bool, error) {
	calls := ckpt.Calls
	_ = sess.Append(event.SessionEventCheckpointRestore, event.CheckpointData{
		Turn: turn, Step: step,
		HookKind:   string(ckpt.HookKind),
		DecisionOK: ckpt.ResolvedAt != nil,
	})
	rt.Notify(agent.Event{Type: agent.EventResume, SessionID: sess.ID, Turn: turn, Content: "resume from checkpoint"})

	outs, suspend := execBatchTools(ctx, rt, sess, turn, step, calls)
	if suspend != nil {
		// 二次挂起（resume 决策未覆盖或新审批）：重写 checkpoint 并冒泡。
		if err := d.saveCheckpoint(ctx, rt, sess, turn, step, calls, suspend); err != nil {
			return "", false, fmt.Errorf("agent: save checkpoint: %w", err)
		}
		return "", false, suspend
	}
	resumeAction := planner.Action{Content: "", ToolCalls: calls}
	return d.finalizeToolBatch(ctx, rt, sess, turn, step, calls, &resumeAction, outs)
}

// finalizeToolBatch 收尾一轮工具批量：Doom Loop 致命检查 → 按源序回填
// （tool/result 日志 + 观测；挂起工具结果错误冒泡）→ step/end → 停止裁决 → 完成。
// 普通路径与 resume 路径共用（架构 v2 §3.9 finalize 阶段）。
func (d *ReActDriver) finalizeToolBatch(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn, step int, calls []llm.ToolCall, action *planner.Action, outs []executor.Outcome[*llm.ToolCall, toolOutcome]) (string, bool, error) {
	// Doom Loop 是致命守卫错误：任务中断（状态 blocked），不回填工具结果
	for _, out := range outs {
		var doomErr *hook.DoomLoopError
		if errors.As(out.Err, &doomErr) {
			sess.State.Status = session.StatusBlocked
			_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: turn, Step: step})
			return "", false, doomErr
		}
	}
	// finalize：按模型调用顺序回填（tool/result 日志 + 观测），Seq 连续、顺序可重放
	for _, out := range outs {
		call := out.Input
		if out.Denied {
			var deniedErr *ToolDeniedError
			if errors.As(out.Err, &deniedErr) {
				_ = sess.AppendToolResult(turn, call, nil, "", deniedErr)
				rt.Notify(agent.Event{Type: agent.EventToolResult, SessionID: sess.ID, Turn: turn, ToolCall: call})
				continue
			}
			// preflight 阶段的非守卫错误（如 tool/call 落日志失败）：终止任务
			return "", false, fmt.Errorf("agent: tool preflight: %w", out.Err)
		}
		// 工具执行路径抛出的挂起（request_human_input 等工具内发起审批）：写 checkpoint 并冒泡
		if out.Output.err != nil {
			var suspend *approval.SuspendError
			if errors.As(out.Output.err, &suspend) {
				if err := d.saveCheckpoint(ctx, rt, sess, turn, step, calls, suspend); err != nil {
					return "", false, fmt.Errorf("agent: save checkpoint: %w", err)
				}
				return "", false, suspend
			}
		}
		_ = sess.AppendToolResult(turn, call, out.Output.result, out.Output.text, out.Output.err)
		rt.Notify(agent.Event{Type: agent.EventToolResult, SessionID: sess.ID, Turn: turn, ToolCall: call, ToolResult: out.Output.result})
	}

	_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: turn, Step: step})

	// 外部停止裁决（对齐 PI shouldStopAfterTurn）：每轮工具执行后调用，
	// 返回 true 时任务以 completed 收尾（reason "stopped"）。
	if rt.ShouldStopAfterTurn != nil {
		stop, err := rt.ShouldStopAfterTurn(ctx, sess, turn, action)
		if err != nil {
			return "", false, fmt.Errorf("agent: shouldStopAfterTurn turn %d: %w", turn, err)
		}
		if stop {
			sess.State.Status = session.StatusCompleted
			_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: turn, Reason: "stopped"})
			rt.Notify(agent.Event{Type: agent.EventDone, SessionID: sess.ID, Turn: turn, Content: action.Content})
			return action.Content, true, nil
		}
	}
	return "", false, nil
}

// nextAction 执行单轮规划。流式路径下转发模型文本增量（EventStream + assistant/chunk）；
// 规划器不支持流式接口时回退非流式 NextAction。
func (d *ReActDriver) nextAction(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, msgs []session.InternalMessage) (planner.Action, error) {
	var action planner.Action
	err := telemetry.WithSpan(ctx, rt.Telemetry, telemetry.SpanLLMRequest,
		map[string]string{"turn": strconv.Itoa(turn), "stream": strconv.FormatBool(rt.Stream)},
		func(ctx context.Context) error {
			var err error
			action, err = d.nextActionInner(ctx, rt, sess, turn, msgs)
			return err
		})
	return action, err
}

// nextActionInner 是 nextAction 的实现（模型调用 + 流式增量转发）。
func (d *ReActDriver) nextActionInner(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, msgs []session.InternalMessage) (planner.Action, error) {
	llmMsgs := session.ConvertToLlm(msgs)
	tools := rt.Tools.List()
	if !rt.Stream {
		return rt.Planner.NextAction(ctx, llmMsgs, tools)
	}
	sp, ok := rt.Planner.(planner.StreamingPlanner)
	if !ok {
		return rt.Planner.NextAction(ctx, llmMsgs, tools)
	}
	return sp.NextActionStream(ctx, llmMsgs, tools, func(chunk llm.StreamChunk) error {
		if chunk.Delta == "" && chunk.Reasoning == "" {
			return nil
		}
		_ = sess.Append(event.SessionEventAssistantChunk, event.AssistantChunkData{Turn: turn, Delta: chunk.Delta, Reasoning: chunk.Reasoning})
		rt.Notify(agent.Event{Type: agent.EventStream, SessionID: sess.ID, Turn: turn, Delta: chunk.Delta, Reasoning: chunk.Reasoning})
		return nil
	})
}

// toolOutcome 是单条工具调用的执行结果（result 为 execute 链原始返回值，
// text 为 post-execute 截断后的模型可见文本，err 为运行期错误）。
type toolOutcome struct {
	result any
	text   string
	err    error
}

// execBatchTools 批量执行工具调用（架构 v2 §3.9 三阶段，对齐 PI executeToolCallsParallel）：
//   - preflight（串行）：单调守卫决策（**先于** tool/call 日志——挂起时不落日志），
//     拒绝 = 拦截集合（显式失败回填）；挂起（*approval.SuspendError）= 整批中止（返回第二返回值）；
//   - execute（有界并发）：pre-execute → execute → post-execute 钩子链；任一工具声明
//     SequentialTool 时整批回退串行（对齐 PI per-tool executionMode）；
//   - finalize（调用方）：结果按模型调用顺序回填（本函数只产出按源序的 Outcome）。
//
// 守卫拒绝返回 *ToolDeniedError（非致命，结果反馈给模型）；DoomLoop 致命错误原样返回
// （调用方终止任务）；其余错误视为运行期失败，进各自 Outcome。
// 返回第二个值 *approval.SuspendError：非 nil 表示有审批待决，整批已中止、未执行任何工具
// （已通过守卫并落日志的先行工具可能留下孤儿 tool/call，不影响模型面投影）。
func execBatchTools(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn, step int, calls []llm.ToolCall) ([]executor.Outcome[*llm.ToolCall, toolOutcome], *approval.SuspendError) {
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
	execute := func(ctx context.Context, call *llm.ToolCall) (toolOutcome, error) {
		// 注入工具执行上下文（request_human_input 等人工介入工具读取）：
		// 当前调用 ID + 审批通道 + resume 决策表。
		ctx = tool.WithCallID(ctx, call.ID)
		if rt.Approval != nil {
			ctx = approval.WithApproval(ctx, rt.Approval)
		}
		if rt.ResumeCheckpoint != nil {
			ctx = approval.WithResumeDecisions(ctx, rt.ResumeCheckpoint.Decisions)
		}
		var out toolOutcome
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
func executeTool(ctx context.Context, rt *driver.RunContext, call *llm.ToolCall, turn, step int) (toolOutcome, error) {
	ev := &hook.HookEvent{Kind: hook.HookToolPreExecute, ToolCall: call, Turn: turn, Step: step}
	ev, err := rt.Hooks.Waterfall(ctx, ev) // pre-execute（可改写/拒绝）
	if err != nil {
		return toolOutcome{err: err}, nil
	}
	execEv := &hook.HookEvent{Kind: hook.HookToolExecute, ToolCall: ev.ToolCall, Turn: turn, Step: step}
	execEv, err = rt.Hooks.Waterfall(ctx, execEv)
	if err != nil {
		return toolOutcome{err: err}, nil
	}
	postEv := &hook.HookEvent{Kind: hook.HookToolPostExecute, ToolCall: ev.ToolCall, Result: execEv.Result, Err: execEv.Err, Turn: turn, Step: step}
	postEv, err = rt.Hooks.Waterfall(ctx, postEv)
	if err != nil {
		return toolOutcome{err: err}, nil
	}
	return toolOutcome{result: execEv.Result, text: postEv.ResultText, err: postEv.Err}, nil
}

// saveCheckpoint 落库挂起点（M2.8，架构 v2 §3.8/§10.6）：
//   - approval/request 事件（durable 审计观测）；
//   - checkpoint 写入 rt.Checkpoints（含挂起轮工具批 + 审批请求 + TaskState 快照）；
//   - 任务状态经 Session.Persistence.SaveState 落盘（跨进程 Resume 还原 Facts）。
//
// 未配置 rt.Checkpoints 时仅记录 approval/request 事件（SuspendError 仍冒泡，无法跨进程恢复）。
func (d *ReActDriver) saveCheckpoint(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn, step int, calls []llm.ToolCall, suspend *approval.SuspendError) error {
	_ = sess.Append(event.SessionEventApprovalRequest, event.ApprovalRequestData{
		ID:       string(suspend.ID),
		ToolName: suspend.Request.ToolName,
		Command:  suspend.Request.Command,
		Reason:   suspend.Request.Reason,
		Args:     suspend.Request.Args,
		HookKind: string(suspend.HookKind),
	})
	if rt.Checkpoints != nil {
		ckpt := &checkpoint.Checkpoint{
			SessionID:       sess.ID,
			Turn:            turn,
			Step:            step,
			HookKind:        suspend.HookKind,
			Calls:           calls,
			Request:         suspend.Request,
			ApprovalID:      suspend.ID,
			SuspendedCallID: suspend.CallID,
			Decisions:       make(map[string]approval.ApprovalDecision),
			Snapshot:        sess.State,
			CreatedAt:       time.Now(),
		}
		if err := rt.Checkpoints.Save(ctx, ckpt); err != nil {
			return fmt.Errorf("agent: save checkpoint: %w", err)
		}
		_ = sess.Append(event.SessionEventCheckpointSave, event.CheckpointData{
			Turn: turn, Step: step, HookKind: string(suspend.HookKind),
		})
	}
	if p := sess.Persistence; p != nil {
		if err := p.SaveState(sess.ID, sess.State); err != nil {
			return fmt.Errorf("agent: save state: %w", err)
		}
	}
	return nil
}

// ---- 默认钩子/守卫 ----

// policyGuard 将 Policy 注入单调守卫链（工具调用前裁决，不可被 hook 改写绕过）。
// 兼承载 M2.8 异步审批：策略返回 *approval.RequiredError（需人工确认）时，
// 走 Approval.Request → 挂起（*approval.SuspendError）/ 立即决策（Immediate）/
// resume 决策表放行（Checkpoint.Decisions[call.ID]）。未配置 Approval 时
// 对需确认操作 fail-closed 拒绝（保持旧版无确认器语义）。
func policyGuard(rt *driver.RunContext) hook.Guard {
	return func(ctx context.Context, ev *hook.HookEvent) error {
		call := ev.ToolCall
		if call == nil {
			return nil
		}
		err := rt.Policy.Allow(ctx, call)
		if err == nil {
			return nil
		}
		var reqErr *approval.RequiredError
		if !errors.As(err, &reqErr) {
			return err // 硬拒绝（路径越界等）
		}
		// 需要人工确认
		if rt.Approval == nil {
			return fmt.Errorf("工具 %q 需要人工确认，但未配置 Approval（fail-closed）", call.Name)
		}
		// resume 决策表：该调用已有回填决策 → 直接应用（不重新发起审批）
		if ckpt := rt.ResumeCheckpoint; ckpt != nil {
			if d, ok := ckpt.Decisions[call.ID]; ok {
				if !d.Approved {
					return &ToolDeniedError{Reason: fmt.Sprintf("用户拒绝执行 %s: %s", call.Name, d.Reason)}
				}
				return nil // 已审批放行
			}
		}
		id, err := rt.Approval.Request(ctx, reqErr.Request)
		if err != nil {
			return err
		}
		// 立即决策（同步确认器适配）：直接应用、不挂起
		if im, ok := rt.Approval.(approval.Immediate); ok {
			if d, found := im.Decision(ctx, id); found {
				if !d.Approved {
					return &ToolDeniedError{Reason: "用户拒绝执行 " + call.Name}
				}
				return nil
			}
		}
		return &approval.SuspendError{
			ID:         id,
			Request:    reqErr.Request,
			CallID:     call.ID,
			HookKind:   hook.HookToolPreExecute,
			ReplayTurn: ev.Turn,
			ReplayStep: ev.Step,
		}
	}
}

// executeHook 是 tool/execute 链尾的默认执行器：实际调用工具并填充结果。
// 平台可在其前注册包装钩子（超时/重试/指标），经 next 委托到本执行器。
func executeHook(rt *driver.RunContext) hook.Hook {
	return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		if ev.ToolCall == nil {
			return next(ctx, ev)
		}
		result, err := rt.Tools.Call(ctx, ev.ToolCall.Name, json.RawMessage(ev.ToolCall.Args))
		ev.Result = result
		if err != nil {
			ev.Err = err
		}
		return next(ctx, ev)
	}
}

// truncateHook 是 tool/post-execute 环节的上下文瘦身：对给模型看的工具结果文本
// 做行数/字节上限截断（超限携带逃生通道提示）；审计仍使用原始 Result。
func truncateHook(rt *driver.RunContext) hook.Hook {
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

// auditHook 是 tool/post-execute 环节的审计门（Audit Gate）：
// 仅将审计器认可的环境事实写入任务状态（TaskState），并广播 EventFact。
func auditHook(rt *driver.RunContext) hook.Hook {
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

// compressHook 是 pre-step 环节的 fresh-context 压缩：两轮之间历史超过阈值时，
// 将旧消息压缩为摘要（TaskState 已验证事实）注入，写 compaction 三段式事件
// （start → summary → end，summary 携带阴影范围，可回放）。
func compressHook(rt *driver.RunContext) hook.Hook {
	return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		if rt.Compactor == nil || ev.Turn <= 1 || len(ev.Messages) == 0 {
			return next(ctx, ev)
		}
		sess := rt.Session
		msgs := ev.Messages
		var system []session.InternalMessage
		hist := msgs
		if msgs[0].Role == llm.RoleSystem {
			system = msgs[:1]
			hist = msgs[1:]
		}
		if len(hist) == 0 {
			return next(ctx, ev)
		}
		after, ok := rt.Compactor.Compact(hist, &sess.State)
		if !ok || len(after) == 0 {
			return next(ctx, ev)
		}
		// 推导被压缩消息的事件序号范围（压缩 = 丢弃历史头部，after = [summary] + kept）
		seqs := session.MessageEventSeqs(sess.Events)
		shadowed := len(hist) - (len(after) - 1)
		var from, to int64
		if shadowed > 0 && shadowed <= len(seqs) {
			from = seqs[0]
			to = seqs[shadowed-1]
		}
		_ = sess.Append(event.SessionEventCompactionStart, event.CompactionStartData{Turn: ev.Turn})
		_ = sess.Append(event.SessionEventCompactionSummary, event.CompactionSummaryData{
			Summary: after[0].Content, ShadowedFrom: from, ShadowedTo: to,
		})
		_ = sess.Append(event.SessionEventCompactionEnd, event.CompactionEndData{Turn: ev.Turn})
		rt.Notify(agent.Event{Type: agent.EventCompacted, SessionID: sess.ID, Turn: ev.Turn, Content: sess.State.Snapshot()})

		ev.Messages = append(system, after...)
		return next(ctx, ev)
	}
}

// ragInjectHook 是 pre-step 环节的 RAG 检索注入：按任务目标检索长期记忆，
// 经知识守门后以 CustomMsg（来源标注 [rag]）注入本轮模型输入（不进日志，
// 每轮按最新知识重检）。
func ragInjectHook(rt *driver.RunContext) hook.Hook {
	return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		if rt.Retriever == nil {
			return next(ctx, ev)
		}
		query := rt.Session.State.Goal
		if query == "" {
			for i := len(ev.Messages) - 1; i >= 0; i-- {
				if ev.Messages[i].Role == llm.RoleUser && !ev.Messages[i].Summary {
					query = ev.Messages[i].Content
					break
				}
			}
		}
		extras, err := retrieveAndInject(ctx, rt, ev.Turn, query)
		if err != nil {
			return nil, err
		}
		if len(extras) > 0 {
			ev.Messages = append(ev.Messages, extras...)
		}
		return next(ctx, ev)
	}
}

// retrieveAndInject 执行 RAG 检索 + 知识守门，返回待发送给模型的注入消息列表。
// 返回的扩展消息不写入会话事件日志（每轮按最新知识重新检索）。
func retrieveAndInject(ctx context.Context, rt *driver.RunContext, turn int, query string) ([]session.InternalMessage, error) {
	limit := rt.RetrieverCfg.Limit
	if limit <= 0 {
		limit = 8
	}
	maxChunks := rt.RetrieverCfg.MaxChunks
	if maxChunks <= 0 {
		maxChunks = 3
	}
	docs, err := rt.Retriever.Retrieve(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("agent: retrieve: %w", err)
	}
	if len(docs) == 0 {
		return nil, nil
	}

	// 冲突裁决：同一来源只保留最高分分块
	bySource := make(map[string]memory.Document, len(docs))
	for _, d := range docs {
		key := d.Source
		if key == "" {
			key = d.ID
		}
		cur, ok := bySource[key]
		if !ok || d.Score > cur.Score {
			bySource[key] = d
		}
	}
	selected := make([]memory.Document, 0, len(bySource))
	for _, d := range bySource {
		selected = append(selected, d)
	}
	sortByScore(selected)

	// 知识守门：低于最低分的分块不注入
	passed := selected[:0]
	for _, d := range selected {
		if rt.RetrieverCfg.MinScore > 0 && d.Score < rt.RetrieverCfg.MinScore {
			continue
		}
		passed = append(passed, d)
	}
	if len(passed) == 0 && rt.RetrieverCfg.MinScore > 0 {
		rt.Notify(agent.Event{Type: agent.EventKnowledgeGap, SessionID: rt.Session.ID, Turn: turn, Content: query})
		return nil, nil
	}
	if len(passed) > maxChunks {
		passed = passed[:maxChunks]
	}
	return []session.InternalMessage{session.CustomMsg("rag", renderRetrieved(passed))}, nil
}

// renderRetrieved 将检索结果渲染为注入文本：逐条标注来源与得分。
func renderRetrieved(docs []memory.Document) string {
	var b strings.Builder
	b.WriteString("检索到的相关记忆：\n")
	for _, d := range docs {
		fmt.Fprintf(&b, "- [来源: %s] (score %.2f): %s\n", d.Source, d.Score, strings.TrimSpace(d.Content))
	}
	return strings.TrimSpace(b.String())
}

// sortByScore 按得分降序排列检索分块。
func sortByScore(docs []memory.Document) {
	for i := 1; i < len(docs); i++ {
		for j := i; j > 0 && docs[j].Score > docs[j-1].Score; j-- {
			docs[j], docs[j-1] = docs[j-1], docs[j]
		}
	}
}

// isTruncatedFinish 判断模型停止原因是否为"输出被截断"。
// openai: length；anthropic: max_tokens；gemini: MAX_TOKENS（已转小写）。
// 截断时已产出的工具调用可能不完整，循环层 fail-closed 处理（见 ReActDriver.Run）。
func isTruncatedFinish(reason string) bool {
	switch strings.ToLower(reason) {
	case "length", "max_tokens":
		return true
	}
	return false
}
