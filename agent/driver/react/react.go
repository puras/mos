// Package react 提供默认 ReAct 循环驱动（架构 v2 §3.3/§11.3）：
// ReActDriver 实现 LoopDriver，NewReAct 注册默认钩子/守卫，New 是默认装配入口。
package react

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
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
		rt.Guards.Add(policyGuard(rt.Policy))
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
	sess := rt.Session
	ctx = tool.WithSandbox(ctx, rt.Sandbox)

	// 任务状态初始化：首次运行记录原始目标，进入 running。
	if sess.State.Goal == "" {
		sess.State.Goal = input
	}
	if sess.State.Status != session.StatusRunning {
		sess.State.Status = session.StatusRunning
	}

	if err := sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: 1, Reason: "user"}); err != nil {
		return "", fmt.Errorf("agent: append turn/start: %w", err)
	}
	if err := sess.AppendUserMessage(input, "user"); err != nil {
		return "", fmt.Errorf("agent: append user/message: %w", err)
	}

	history := sess.Messages() // 不含 system（system 每轮注入，不写日志）

	for turn := 1; ; turn++ {
		const step = 1

		// 可选保险丝（默认关闭，对齐 DSH/PI 无轮次上限）：显式设置 >0 时兜底防失控。
		if rt.MaxTurns > 0 && turn > rt.MaxTurns {
			sess.State.Status = session.StatusBlocked
			_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: rt.MaxTurns, Reason: "max_turns"})
			return "", fmt.Errorf("agent: max turns %d exceeded", rt.MaxTurns)
		}
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("agent: run canceled: %w", err)
		}

		// 每轮刷新系统提示词：状态槽位随 TaskState 进度更新（M2.6 动态组装）
		sysText, err := rt.AssembleSystem(ctx)
		if err != nil {
			return "", fmt.Errorf("agent: assemble system: %w", err)
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
			return "", fmt.Errorf("agent: pre-step hooks: %w", err)
		}

		if err := sess.Append(event.SessionEventStepStart, event.StepData{Turn: turn, Step: step}); err != nil {
			return "", fmt.Errorf("agent: append step/start: %w", err)
		}

		action, err := d.nextAction(ctx, rt, sess, turn, hev.Messages)
		if err != nil {
			return "", fmt.Errorf("agent: plan turn %d: %w", turn, err)
		}

		calls := action.ToolCalls
		if len(calls) == 0 && action.ToolCall != nil {
			calls = []llm.ToolCall{*action.ToolCall} // 兼容仅填 ToolCall 的第三方规划器
		}
		if err := sess.AppendAssistantMessage(turn, action.Content, action.ReasoningContent, calls, action.Usage); err != nil {
			return "", fmt.Errorf("agent: append assistant/message: %w", err)
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
			return action.Content, nil
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
			history = sess.Messages()
			continue
		}

		// 工具调用
		// 注：多工具批量执行（并发 + 按序回填）属 M3 规划（ConcurrentExecutor），
		// 当前单调用路径执行第一个，其余由规划器在后续轮次继续产出。
		call := &calls[0]
		if err := sess.AppendToolCall(turn, call); err != nil {
			return "", fmt.Errorf("agent: append tool/call: %w", err)
		}
		rt.Notify(agent.Event{Type: agent.EventToolCall, SessionID: sess.ID, Turn: turn, ToolCall: call})

		// 执行工具：守卫（单调）→ pre-execute → execute → post-execute
		tev := &hook.HookEvent{Kind: hook.HookToolPreExecute, ToolCall: call, Turn: turn, Step: step}
		tev, execErr := executeToolChain(ctx, rt, tev)

		var deniedErr *ToolDeniedError
		denied := errors.As(execErr, &deniedErr)
		switch {
		case denied:
			_ = sess.AppendToolResult(turn, call, nil, deniedErr.Error(), nil)
			rt.Notify(agent.Event{Type: agent.EventToolResult, SessionID: sess.ID, Turn: turn, ToolCall: call})
		case execErr != nil:
			// Doom Loop 是致命守卫错误：任务中断（状态 blocked），不回填工具结果
			var doomErr *hook.DoomLoopError
			if errors.As(execErr, &doomErr) {
				sess.State.Status = session.StatusBlocked
				_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: turn, Step: step})
				return "", doomErr
			}
			_ = sess.AppendToolResult(turn, call, nil, "", execErr)
		default:
			_ = sess.AppendToolResult(turn, call, tev.Result, tev.ResultText, tev.Err)
		}

		_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: turn, Step: step})
		history = sess.Messages()

		// 外部停止裁决（对齐 PI shouldStopAfterTurn）：每轮工具执行后调用，
		// 返回 true 时任务以 completed 收尾（reason "stopped"）。
		if rt.ShouldStopAfterTurn != nil {
			stop, err := rt.ShouldStopAfterTurn(ctx, sess, turn, &action)
			if err != nil {
				return "", fmt.Errorf("agent: shouldStopAfterTurn turn %d: %w", turn, err)
			}
			if stop {
				sess.State.Status = session.StatusCompleted
				_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: turn, Reason: "stopped"})
				rt.Notify(agent.Event{Type: agent.EventDone, SessionID: sess.ID, Turn: turn, Content: action.Content})
				return action.Content, nil
			}
		}
	}
}

// nextAction 执行单轮规划。流式路径下转发模型文本增量（EventStream + assistant/chunk）；
// 规划器不支持流式接口时回退非流式 NextAction。
func (d *ReActDriver) nextAction(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, msgs []session.InternalMessage) (planner.Action, error) {
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

// executeToolChain 执行工具调用环节：单调守卫 → tool/pre-execute → tool/execute → tool/post-execute。
// 守卫拒绝返回 *ToolDeniedError（非致命，结果反馈给模型）；其余错误视为运行期失败。
func executeToolChain(ctx context.Context, rt *driver.RunContext, ev *hook.HookEvent) (*hook.HookEvent, error) {
	if err := rt.Guards.Allow(ctx, ev); err != nil {
		var doomErr *hook.DoomLoopError
		if errors.As(err, &doomErr) {
			return nil, doomErr // 致命守卫错误：循环层终止任务（状态 blocked）
		}
		return nil, &ToolDeniedError{Reason: err.Error()}
	}
	ev, err := rt.Hooks.Waterfall(ctx, ev)
	if err != nil {
		return nil, err
	}
	execEv := &hook.HookEvent{Kind: hook.HookToolExecute, ToolCall: ev.ToolCall, Turn: ev.Turn, Step: ev.Step}
	execEv, err = rt.Hooks.Waterfall(ctx, execEv)
	if err != nil {
		return nil, err
	}
	// 广播工具结果观测（在审计之前，保持事件序列 ToolCall → ToolResult → Fact）
	rt.Notify(agent.Event{Type: agent.EventToolResult, SessionID: rt.Session.ID, Turn: execEv.Turn, ToolCall: execEv.ToolCall, ToolResult: execEv.Result})
	postEv := &hook.HookEvent{Kind: hook.HookToolPostExecute, ToolCall: ev.ToolCall, Result: execEv.Result, Err: execEv.Err, Turn: ev.Turn, Step: ev.Step}
	return rt.Hooks.Waterfall(ctx, postEv)
}

// ---- 默认钩子/守卫 ----

// policyGuard 将 Policy 注入单调守卫链（工具调用前裁决，不可被 hook 改写绕过）。
func policyGuard(p driver.Policy) hook.Guard {
	return func(ctx context.Context, ev *hook.HookEvent) error {
		if ev.ToolCall == nil {
			return nil
		}
		return p.Allow(ctx, ev.ToolCall)
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
		for _, f := range facts {
			rt.Session.State.AddFact(f)
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
