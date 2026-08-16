// Package reflection 提供 Basic Reflection 循环驱动（架构 v2 §3.3，Agentic Design
// Patterns 自查模式）：Generate → Reflect → Revise 直至评审通过或达到最大迭代。
//
// 与 ReActDriver 的关系：Generate/Revise 复用内嵌 ReAct 驱动（支持工具），
// Reflect 使用独立评审器（planner.Reflector，独立上下文避免"自我合理化"污染）。
package reflection

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

// defaultMaxIterations 是默认最大评审-修订迭代数（一次生成 + 至多 N 轮修订）。
const defaultMaxIterations = 2

// ReflectionDriver 是"生成-反思-修订"自查循环驱动。
type ReflectionDriver struct {
	generator     driver.LoopDriver // 初稿生成/修订执行器（默认 ReActDriver）
	reflector     planner.Reflector // 评审器（独立上下文）
	maxIterations int               // 最大修订轮数（>0）
}

// Option 是 ReflectionDriver 的装配选项。
type Option func(*ReflectionDriver)

// WithGenerator 替换生成/修订执行器（默认 react.ReActDriver）。
func WithGenerator(d driver.LoopDriver) Option {
	return func(r *ReflectionDriver) { r.generator = d }
}

// WithReflector 注入评审器（默认取 rt.Reflector；均未设置时拒绝运行）。
func WithReflector(p planner.Reflector) Option {
	return func(r *ReflectionDriver) { r.reflector = p }
}

// WithMaxIterations 设置最大修订轮数（默认 2）。
func WithMaxIterations(n int) Option {
	return func(r *ReflectionDriver) {
		if n > 0 {
			r.maxIterations = n
		}
	}
}

// New 返回默认装配的 Reflection 智能体：
// 默认钩子/守卫装配与 react.New 一致（同一 rt 只应由一个驱动入口装配，
// 避免默认钩子重复注册）；生成器为内嵌 ReActDriver，评审器取 rt.Reflector。
func New(rt *driver.RunContext, opts ...Option) *driver.Agent {
	d := &ReflectionDriver{maxIterations: defaultMaxIterations}
	for _, o := range opts {
		o(d)
	}
	if d.generator == nil {
		d.generator = &react.ReActDriver{}
		react.NewReAct(rt)
	}
	if d.reflector == nil {
		d.reflector = rt.Reflector
	}
	return driver.New(rt, d)
}

// Run 实现 LoopDriver：Generate → Reflect → Revise 自查循环。
func (d *ReflectionDriver) Run(ctx context.Context, rt *driver.RunContext, input string) (string, error) {
	if rt == nil || rt.Session == nil {
		return "", fmt.Errorf("agent: run requires session")
	}
	if rt.Tools == nil {
		return "", fmt.Errorf("agent: no tool registry configured")
	}
	// 裸实例惰性装配：Router 等场景直接构造 ReflectionDriver 时补齐生成执行器
	// （ReActDriver 的 Run 会完成默认钩子/守卫装配）。
	if d.generator == nil {
		d.generator = &react.ReActDriver{}
	}
	if d.reflector == nil {
		d.reflector = rt.Reflector
	}
	if d.reflector == nil {
		return "", fmt.Errorf("agent: reflection requires a reflector (rt.Reflector or WithReflector)")
	}
	sess := rt.Session
	ctx = tool.WithSandbox(ctx, rt.Sandbox)

	if sess.State.Goal == "" {
		sess.State.Goal = input
	}
	if sess.State.Status != session.StatusRunning {
		sess.State.Status = session.StatusRunning
	}

	// 任务回合由内嵌执行器（Generate 阶段）开启：ReAct 会写 turn/start +
	// user/message(input)，此处不再重复；reflection 事件统一以起始 Turn 标记。
	turn := sess.NextTurn()

	// Generate：内嵌执行器产出初稿。
	_ = sess.Append(event.SessionEventReflectionStart, event.ReflectionStartData{
		Turn: turn, Goal: input, MaxIterations: d.maxIterations,
	})
	draft, err := d.generator.Run(ctx, rt, input)
	if err != nil {
		return d.fail(sess, turn, 0, err)
	}

	// Reflect → Revise 循环：评审通过或达到最大迭代即收尾。
	for iter := 1; iter <= d.maxIterations; iter++ {
		review, err := d.reflector.Reflect(ctx, input, draft, session.ConvertToLlm(sess.Messages()))
		if err != nil {
			return d.fail(sess, turn, iter, err)
		}
		_ = sess.Append(event.SessionEventReflectionReview, event.ReflectionReviewData{
			Turn: turn, Iteration: iter, Issues: review.Issues, Score: review.Score, Passed: len(review.Issues) == 0,
		})
		rt.Notify(agent.Event{Type: agent.EventReflection, SessionID: sess.ID, Turn: turn, Content: reviewText(iter, review)})

		if len(review.Issues) == 0 {
			return d.finish(sess, rt, turn, iter, "completed", draft)
		}

		// Revise：评审意见写入日志（Model-visible means logged），再让执行器修订。
		_ = sess.AppendUserMessage(reviewText(iter, review), "reflection")
		draft, err = d.generator.Run(ctx, rt, "请根据上面的评审意见修订产出，直接输出修订后的完整内容。")
		if err != nil {
			return d.fail(sess, turn, iter, err)
		}
	}
	return d.finish(sess, rt, turn, d.maxIterations, "max_iterations", draft)
}

// finish 正常收尾：reflection/end + 状态 completed + turn/end + EventDone。
func (d *ReflectionDriver) finish(sess *session.Session, rt *driver.RunContext, turn, iter int, status, draft string) (string, error) {
	_ = sess.Append(event.SessionEventReflectionEnd, event.ReflectionEndData{Turn: turn, Iteration: iter, Status: status})
	sess.State.Status = session.StatusCompleted
	rt.Notify(agent.Event{Type: agent.EventDone, SessionID: sess.ID, Turn: turn, Content: draft})
	return draft, nil
}

// fail 失败收尾：reflection/end failed + 状态 blocked。
func (d *ReflectionDriver) fail(sess *session.Session, turn, iter int, err error) (string, error) {
	_ = sess.Append(event.SessionEventReflectionEnd, event.ReflectionEndData{Turn: turn, Iteration: iter, Status: "failed", Detail: err.Error()})
	sess.State.Status = session.StatusBlocked
	return "", fmt.Errorf("agent: reflection: %w", err)
}

// reviewText 将评审结果渲染为文本（EventReflection.Content / 修订输入）。
func reviewText(iter int, review planner.Review) string {
	var b strings.Builder
	fmt.Fprintf(&b, "评审意见（第 %d 轮）", iter)
	if review.Score >= 0 {
		fmt.Fprintf(&b, "，质量分 %d", review.Score)
	}
	b.WriteString("：\n")
	for i, issue := range review.Issues {
		fmt.Fprintf(&b, "%d. %s\n", i+1, issue)
	}
	return strings.TrimSpace(b.String())
}
