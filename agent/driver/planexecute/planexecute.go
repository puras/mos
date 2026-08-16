// Package planexecute 提供 Plan&Execute 循环驱动（架构 v2 §3.3，Plan-and-Solve 模式）：
// 写计划 → 逐条执行（内嵌 ReActDriver 作为执行器）→ 校验交付物。
//
// 与 ReActDriver 的关系：本包是同一 RunContext 上的另一种 LoopDriver 实现，
// 共享会话、工具、钩子、守卫与审计；执行阶段复用 react 驱动逐条执行计划步骤。
package planexecute

import (
	"context"
	"fmt"
	"strings"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/driver/react"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// VerifyFunc 校验计划交付物（可选，架构 v2 §3.3"校验交付物"）：
// 全部步骤执行完成后调用，返回 false 时任务以 blocked 收尾（plan/end failed）。
// 可基于会话历史 / TaskState / 外部检查做业务裁决；nil 时默认"步骤全完成即通过"。
type VerifyFunc func(ctx context.Context, rt *driver.RunContext, plan planner.Plan, executed []planner.PlanStep) (bool, string)

// PlanExecuteDriver 是"计划-执行"循环驱动。
type PlanExecuteDriver struct {
	executor driver.LoopDriver // 步骤执行器（默认 ReActDriver）
	planner  planner.PlanPlanner
	verify   VerifyFunc
}

// Option 是 PlanExecuteDriver 的装配选项。
type Option func(*PlanExecuteDriver)

// WithExecutor 替换步骤执行器（默认 react.ReActDriver；可注入 PlanExecute 嵌套等）。
func WithExecutor(d driver.LoopDriver) Option {
	return func(p *PlanExecuteDriver) { p.executor = d }
}

// WithPlanner 注入计划规划器（默认取 rt.PlanPlanner；均未设置时退化为单步计划）。
func WithPlanner(p planner.PlanPlanner) Option {
	return func(d *PlanExecuteDriver) { d.planner = p }
}

// WithVerifier 注入交付物校验回调（nil 时默认通过）。
func WithVerifier(v VerifyFunc) Option {
	return func(d *PlanExecuteDriver) { d.verify = v }
}

// New 返回默认装配的 Plan&Execute 智能体：
// 默认钩子/守卫装配与 react.New 一致（同一 rt 只应由一个驱动入口装配，
// 避免默认钩子重复注册）；执行器为内嵌 ReActDriver。
func New(rt *driver.RunContext, opts ...Option) *driver.Agent {
	d := &PlanExecuteDriver{}
	for _, o := range opts {
		o(d)
	}
	if d.executor == nil {
		// 内嵌执行器：与默认装配共享同一 rt 的钩子/守卫/工具/审计。
		d.executor = &react.ReActDriver{}
		react.NewReAct(rt)
	}
	return driver.New(rt, d)
}

// Run 实现 LoopDriver：计划 → 逐条执行 → 校验交付物。
func (d *PlanExecuteDriver) Run(ctx context.Context, rt *driver.RunContext, input string) (string, error) {
	if rt == nil || rt.Session == nil {
		return "", fmt.Errorf("agent: run requires session")
	}
	if rt.Tools == nil {
		return "", fmt.Errorf("agent: no tool registry configured")
	}
	// 裸实例惰性装配：Router 等场景直接构造 PlanExecuteDriver 时补齐内嵌执行器
	// （ReActDriver 的 Run 会完成默认钩子/守卫装配）。
	if d.executor == nil {
		d.executor = &react.ReActDriver{}
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

	turn := sess.NextTurn()
	if err := sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: turn, Reason: "user"}); err != nil {
		return "", fmt.Errorf("agent: append turn/start: %w", err)
	}
	if err := sess.AppendUserMessage(input, "user"); err != nil {
		return "", fmt.Errorf("agent: append user/message: %w", err)
	}

	// 计划阶段：生成步骤列表（无规划器时整体作为单步计划）。
	plan, err := d.makePlan(ctx, rt, sess, input)
	if err != nil {
		return "", err
	}
	_ = sess.Append(event.SessionEventPlanStart, event.PlanStartData{Turn: turn, Goal: plan.Goal, Steps: len(plan.Steps)})
	rt.Notify(agent.Event{Type: agent.EventPlan, SessionID: sess.ID, Turn: turn, Content: planText(plan)})
	for i := range plan.Steps {
		_ = sess.Append(event.SessionEventPlanStep, event.PlanStepData{
			Turn: turn, Index: plan.Steps[i].Index, Summary: plan.Steps[i].Summary, Status: "pending",
		})
	}

	// 执行阶段：逐条执行（内嵌执行器每步一个任务回合，Turn 全局递增）。
	var last string
	for i := range plan.Steps {
		step := plan.Steps[i]
		if err := ctx.Err(); err != nil {
			return d.fail(ctx, sess, turn, step, err)
		}
		_ = sess.Append(event.SessionEventPlanStep, event.PlanStepData{
			Turn: turn, Index: step.Index, Summary: step.Summary, Status: "running",
		})
		out, runErr := d.executor.Run(ctx, rt, step.Summary)
		if runErr != nil {
			return d.fail(ctx, sess, turn, step, runErr)
		}
		_ = sess.Append(event.SessionEventPlanStep, event.PlanStepData{
			Turn: turn, Index: step.Index, Summary: step.Summary, Status: "done",
		})
		last = out
	}

	// 校验交付物（可选 Verify；默认通过）。
	if d.verify != nil {
		ok, detail := d.verify(ctx, rt, plan, plan.Steps)
		if !ok {
			return d.fail(ctx, sess, turn, planner.PlanStep{Index: 0, Summary: "verify"}, fmt.Errorf("%s", detail))
		}
	}

	_ = sess.Append(event.SessionEventPlanEnd, event.PlanEndData{Turn: turn, Status: "completed"})
	sess.State.Status = session.StatusCompleted
	_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: turn, Reason: "done"})
	rt.Notify(agent.Event{Type: agent.EventDone, SessionID: sess.ID, Turn: turn, Content: last})
	return last, nil
}

// makePlan 生成执行计划：优先级 WithPlanner → rt.PlanPlanner → 单步退化。
func (d *PlanExecuteDriver) makePlan(ctx context.Context, rt *driver.RunContext, sess *session.Session, input string) (planner.Plan, error) {
	pp := d.planner
	if pp == nil {
		pp = rt.PlanPlanner
	}
	if pp == nil {
		return planner.Plan{Goal: input, Steps: []planner.PlanStep{{Index: 1, Summary: input}}}, nil
	}
	plan, err := pp.Plan(ctx, input, session.ConvertToLlm(sess.Messages()))
	if err != nil {
		return planner.Plan{}, fmt.Errorf("agent: plan: %w", err)
	}
	if plan.Goal == "" {
		plan.Goal = input
	}
	if len(plan.Steps) == 0 {
		plan.Steps = []planner.PlanStep{{Index: 1, Summary: input}}
	}
	return plan, nil
}

// fail 记录步骤失败并收尾：plan/step failed + plan/end failed + 状态 blocked。
func (d *PlanExecuteDriver) fail(ctx context.Context, sess *session.Session, turn int, step planner.PlanStep, err error) (string, error) {
	_ = sess.Append(event.SessionEventPlanStep, event.PlanStepData{
		Turn: turn, Index: step.Index, Summary: step.Summary, Status: "failed",
	})
	_ = sess.Append(event.SessionEventPlanEnd, event.PlanEndData{Turn: turn, Status: "failed", Detail: err.Error()})
	sess.State.Status = session.StatusBlocked
	if step.Index > 0 {
		return "", fmt.Errorf("agent: plan step %d (%s): %w", step.Index, step.Summary, err)
	}
	return "", err
}

// planText 将计划渲染为观测文本（EventPlan.Content）。
func planText(plan planner.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "计划（%d 步）：\n", len(plan.Steps))
	for _, s := range plan.Steps {
		fmt.Fprintf(&b, "%d. %s\n", s.Index, s.Summary)
	}
	return strings.TrimSpace(b.String())
}
