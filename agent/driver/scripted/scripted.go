// Package scripted 提供脚本化编排驱动（架构 v2 §3.3 PTC 模式）：
// 写脚本（依赖 DAG）→ 分波有界并发执行（纯工具调用，0 次额外模型调用）→ 合成最终回复。
// 全程 2 次模型调用；工具执行经 agent/execpath 共享通道（与 react 批量语义一致）。
package scripted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/checkpoint"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/agent/telemetry"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/sandbox"
)

// DefaultMaxSteps 是脚本步数上限（防脚本爆炸 + 防提示词膨胀）。
const DefaultMaxSteps = 16

// ScriptedDriver 是脚本化编排循环驱动。
type ScriptedDriver struct {
	planner  planner.ScriptPlanner
	maxSteps int
}

// Option 是 ScriptedDriver 的装配选项。
type Option func(*ScriptedDriver)

// WithPlanner 注入脚本规划器（默认取 rt.ScriptPlanner；均未设置时拒绝运行）。
func WithPlanner(p planner.ScriptPlanner) Option {
	return func(d *ScriptedDriver) { d.planner = p }
}

// WithMaxSteps 覆盖脚本步数上限（默认 DefaultMaxSteps）。
func WithMaxSteps(n int) Option {
	return func(d *ScriptedDriver) { d.maxSteps = n }
}

// New 返回默认装配的脚本化编排智能体：
// 默认钩子/守卫与 react.New 一致（复用 react.InstallDefaults），但不注册 Doom Loop 守卫
// （脚本中故意重复同一工具调用是合法语义）；规划器取 WithPlanner 或 rt.ScriptPlanner，
// 均未设置时由 Run 阶段 fail-closed 拒绝运行。
func New(rt *driver.RunContext, opts ...Option) *driver.Agent {
	d := &ScriptedDriver{maxSteps: DefaultMaxSteps}
	for _, o := range opts {
		o(d)
	}
	react.InstallDefaults(rt)
	return driver.New(rt, d)
}

// Run 实现 LoopDriver：脚本轮（写脚本 + 分波执行）→ 合成轮（模型产出最终回复）。
func (d *ScriptedDriver) Run(ctx context.Context, rt *driver.RunContext, input string) (string, error) {
	if rt == nil || rt.Session == nil {
		return "", errors.New("agent: run requires session")
	}
	if rt.Tools == nil {
		return "", errors.New("agent: no tool registry configured")
	}
	if rt.Planner == nil {
		return "", errors.New("agent: scripted driver requires a Planner")
	}
	if d.planner == nil && rt.ScriptPlanner == nil {
		return "", errors.New("agent: scripted driver requires a ScriptPlanner")
	}
	// 裸实例惰性默认装配（Router 等场景直接构造 ScriptedDriver 时补齐）。
	if rt.Hooks == nil || rt.Guards == nil {
		react.InstallDefaults(rt)
	}
	sess := rt.Session
	ctx = sandbox.WithRunner(ctx, rt.Runner)

	if sess.State.Goal == "" {
		sess.State.Goal = input
	}
	if sess.State.Status != session.StatusRunning {
		sess.State.Status = session.StatusRunning
	}

	firstTurn := sess.NextTurn()
	var plan planner.ScriptedPlan
	var st *scriptState
	var resumeCalls []llm.ToolCall

	if ckpt := rt.ResumeCheckpoint; ckpt != nil {
		// resume：脚本 + 状态从 checkpoint 恢复，跳过计划与事件重放。
		firstTurn = ckpt.Turn
		_ = sess.Append(event.SessionEventCheckpointRestore, event.CheckpointData{
			Turn: ckpt.Turn, Step: 1, HookKind: string(ckpt.HookKind), DecisionOK: ckpt.ResolvedAt != nil,
		})
		rt.Notify(agent.Event{Type: agent.EventResume, SessionID: sess.ID, Turn: ckpt.Turn, Content: "resume scripted from checkpoint"})
		if ckpt.ScriptState == nil {
			return "", errors.New("agent: scripted resume requires ScriptState in checkpoint")
		}
		plan = ckpt.ScriptState.Plan
		st = restoreState(&plan, ckpt.ScriptState)
		resumeCalls = ckpt.Calls
	} else {
		if err := sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: firstTurn, Reason: "user"}); err != nil {
			return "", fmt.Errorf("agent: append turn/start: %w", err)
		}
		if err := sess.AppendUserMessage(input, "user"); err != nil {
			return "", fmt.Errorf("agent: append user/message: %w", err)
		}

		// 脚本阶段
		var err error
		plan, err = d.makeScript(ctx, rt, sess, input)
		if err != nil {
			return "", err
		}
		if err := validateScript(&plan, rt.Tools, d.maxSteps); err != nil {
			return "", fmt.Errorf("agent: script rejected: %w", err)
		}
		st = newState(&plan)
		_ = sess.Append(event.SessionEventScriptStart, event.ScriptStartData{Turn: firstTurn, Goal: plan.Goal, Steps: len(plan.Steps)})
		rt.Notify(agent.Event{Type: agent.EventScript, SessionID: sess.ID, Turn: firstTurn, Content: scriptText(&plan)})
		_ = sess.AppendAssistantMessage(firstTurn, scriptText(&plan), "", nil, llm.Usage{})
		for i := range plan.Steps {
			_ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{
				Turn: firstTurn, StepID: plan.Steps[i].ID, Tool: plan.Steps[i].Tool, Status: statusPending,
			})
		}
	}

	// 执行阶段（分波）
	var report *scriptReport
	var suspend *approval.SuspendError
	var execErr error
	_ = telemetry.WithSpan(ctx, rt.Telemetry, telemetry.SpanDriverTurn,
		map[string]string{"turn": strconv.Itoa(firstTurn), "driver": "scripted"},
		func(ctx context.Context) error {
			report, suspend, execErr = executeWaves(ctx, rt, sess, firstTurn, &plan, st, resumeCalls)
			return nil
		})
	if suspend != nil {
		if err := d.saveCheckpoint(ctx, rt, sess, firstTurn, &plan, st, suspend); err != nil {
			return "", fmt.Errorf("agent: save checkpoint: %w", err)
		}
		return "", suspend
	}
	if execErr != nil {
		return "", execErr
	}

	// script/end 携带执行报告：每步状态 + 命名输出摘要（架构 v2 §6 契约）
	scriptEnd := event.ScriptEndData{Turn: firstTurn, Status: "completed"}
	if report != nil {
		scriptEnd.Steps = report.Status
		scriptEnd.Outputs = report.Outputs
	}
	_ = sess.Append(event.SessionEventScriptEnd, scriptEnd)
	_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: firstTurn, Step: 1})
	_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: firstTurn, Reason: "done"})

	// 合成轮：模型看到全部工具结果，产出最终回复。
	return d.synthesize(ctx, rt, sess)
}

// makeScript 生成脚本：WithPlanner → rt.ScriptPlanner → 拒绝运行。
func (d *ScriptedDriver) makeScript(ctx context.Context, rt *driver.RunContext, sess *session.Session, input string) (planner.ScriptedPlan, error) {
	p := d.planner
	if p == nil {
		p = rt.ScriptPlanner
	}
	plan, err := p.Script(ctx, input, session.ConvertToLlm(sess.Messages()), rt.Tools.List())
	if err != nil {
		return planner.ScriptedPlan{}, fmt.Errorf("agent: script: %w", err)
	}
	if plan.Goal == "" {
		plan.Goal = input
	}
	return plan, nil
}

// synthesize 合成轮：一次 NextAction（不注入工具，防模型继续调用）；
// 模型仍返回工具调用时 fail-closed（显式失败回填 + 报错），不执行、不进多脚本循环。
func (d *ScriptedDriver) synthesize(ctx context.Context, rt *driver.RunContext, sess *session.Session) (string, error) {
	synTurn := sess.NextTurn()
	_ = sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: synTurn, Reason: "continuation"})

	msgs := make([]session.InternalMessage, 0, 2)
	if sysText, err := rt.AssembleSystem(ctx); err != nil {
		return "", fmt.Errorf("agent: assemble system: %w", err)
	} else if sysText != "" {
		msgs = append(msgs, session.SystemMsg(sysText))
	}
	msgs = append(msgs, sess.Messages()...)

	hev := &hook.HookEvent{Kind: hook.HookPreStep, Messages: msgs, Turn: synTurn, Step: 1}
	hev, err := rt.Hooks.Waterfall(ctx, hev)
	if err != nil {
		return "", fmt.Errorf("agent: pre-step hooks: %w", err)
	}

	action, err := d.nextAction(ctx, rt, sess, synTurn, hev.Messages)
	if err != nil {
		return "", fmt.Errorf("agent: synthesize turn %d: %w", synTurn, err)
	}
	calls := action.ToolCalls
	if len(calls) == 0 && action.ToolCall != nil {
		calls = []llm.ToolCall{*action.ToolCall} // 兼容仅填 ToolCall 的第三方规划器
	}
	if err := sess.AppendAssistantMessage(synTurn, action.Content, action.ReasoningContent, calls, action.Usage); err != nil {
		return "", fmt.Errorf("agent: append assistant/message: %w", err)
	}
	rt.MonitorUsage(synTurn, action.Usage)
	if action.Content != "" {
		rt.Notify(agent.Event{Type: agent.EventStep, SessionID: sess.ID, Turn: synTurn, Content: action.Content})
	}

	if len(calls) > 0 {
		// fail-closed：合成轮不允许工具调用（v1 语义，见设计 §6）
		err := errors.New("agent: scripted synthesis must answer directly (no tool calls)")
		for i := range calls {
			_ = sess.AppendToolResult(synTurn, &calls[i], nil, "", err)
		}
		sess.State.Status = session.StatusBlocked
		_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: synTurn, Reason: "synthesis_tool_calls"})
		return "", err
	}
	if action.Content == "" {
		sess.State.Status = session.StatusBlocked
		_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: synTurn, Reason: "empty"})
		return "", errors.New("agent: scripted synthesis produced no reply")
	}

	sess.State.Status = session.StatusCompleted
	_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: synTurn, Reason: "done"})
	rt.Notify(agent.Event{Type: agent.EventDone, SessionID: sess.ID, Turn: synTurn, Content: action.Content})
	return action.Content, nil
}

// nextAction 合成轮规划（非流式；流式路径下转发增量，复用 react 语义）。
func (d *ScriptedDriver) nextAction(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, msgs []session.InternalMessage) (planner.Action, error) {
	llmMsgs := session.ConvertToLlm(msgs)
	if !rt.Stream {
		return rt.Planner.NextAction(ctx, llmMsgs, nil)
	}
	sp, ok := rt.Planner.(planner.StreamingPlanner)
	if !ok {
		return rt.Planner.NextAction(ctx, llmMsgs, nil)
	}
	return sp.NextActionStream(ctx, llmMsgs, nil, func(chunk llm.StreamChunk) error {
		if chunk.Delta == "" && chunk.Reasoning == "" {
			return nil
		}
		_ = sess.Append(event.SessionEventAssistantChunk, event.AssistantChunkData{Turn: turn, Delta: chunk.Delta, Reasoning: chunk.Reasoning})
		rt.Notify(agent.Event{Type: agent.EventStream, SessionID: sess.ID, Turn: turn, Delta: chunk.Delta, Reasoning: chunk.Reasoning})
		return nil
	})
}

// restoreState 从 checkpoint 恢复脚本执行状态（resume）。Outputs/Status 深拷贝：
// 不直接复用 checkpoint 的 map（MemStore.Load 是浅拷贝 cp := *ckpt），否则续跑期
// mutate outputs/status 会改写存储的 ScriptState，跨进程 Resume 读到旧值丢状态。
func restoreState(plan *planner.ScriptedPlan, ss *planner.ScriptState) *scriptState {
	st := newState(plan)
	for id, s := range ss.Status {
		st.Status[id] = s
	}
	for id, v := range ss.Outputs {
		st.Outputs[id] = deepCopyValue(v)
	}
	return st
}

// deepCopyValue 深拷贝脚本输出的容器值（map[string]any / []any）：
// 工具输出是 JSON 形态（容器 + 标量），标量不可变、复制安全；
// 容器逐层新建，切掉与 checkpoint 快照的共享引用（对应 restoreState 的深拷贝语义）。
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopyValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopyValue(vv)
		}
		return out
	default:
		return v
	}
}

// saveCheckpoint 落库挂起点（M2.8）：脚本状态 + 挂起波工具调用 + 审批请求 + TaskState 快照。
func (d *ScriptedDriver) saveCheckpoint(ctx context.Context, rt *driver.RunContext, sess *session.Session, turn int, plan *planner.ScriptedPlan, st *scriptState, suspend *approval.SuspendError) error {
	_ = sess.Append(event.SessionEventApprovalRequest, event.ApprovalRequestData{
		ID: string(suspend.ID), ToolName: suspend.Request.ToolName,
		Command: suspend.Request.Command, Reason: suspend.Request.Reason, Args: suspend.Request.Args,
		HookKind: string(suspend.HookKind),
	})
	if rt.Checkpoints != nil {
		ckpt := &checkpoint.Checkpoint{
			SessionID: sess.ID, Turn: turn, Step: 1,
			HookKind:        suspend.HookKind,
			Calls:           st.ResumeCalls,
			Request:         suspend.Request,
			ApprovalID:      suspend.ID,
			SuspendedCallID: suspend.CallID,
			Decisions:       make(map[string]approval.ApprovalDecision),
			Snapshot:        sess.State,
			ScriptState: &planner.ScriptState{
				Plan:    *plan,
				Outputs: st.Outputs,
				Status:  st.Status,
			},
			CreatedAt: time.Now(),
		}
		if err := rt.Checkpoints.Save(ctx, ckpt); err != nil {
			return fmt.Errorf("agent: save checkpoint: %w", err)
		}
		_ = sess.Append(event.SessionEventCheckpointSave, event.CheckpointData{
			Turn: turn, Step: 1, HookKind: string(suspend.HookKind),
		})
	}
	if p := sess.Persistence; p != nil {
		if err := p.SaveState(sess.ID, sess.State); err != nil {
			return fmt.Errorf("agent: save state: %w", err)
		}
	}
	return nil
}

// scriptText 将脚本渲染为观测文本（EventScript.Content / assistant message）。
func scriptText(plan *planner.ScriptedPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "脚本（%d 步）：\n", len(plan.Steps))
	for i := range plan.Steps {
		st := &plan.Steps[i]
		deps := strings.Join(st.DependsOn, ",")
		line := fmt.Sprintf("%s. [%s] %s", st.ID, st.Tool, formatArgs(st.Args))
		if deps != "" {
			line += "（依赖: " + deps + "）"
		}
		if st.Output != "" {
			line += " → " + st.Output
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimSpace(b.String())
}

func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(args)
	return string(b)
}
