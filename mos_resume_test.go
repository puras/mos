package mos_test

// 续跑（FR-G3 / S9.2）装配层 E2E：宿主显式指示 → 悬空调用走完 → 本轮正常收尾。
//
// 纪律（与设计一致）：
//   - **默认不续**：不调 Resume 就没人续（挂起的轮次一直挂着）；
//   - 沿用**原轮次号**：审批决定的复用键是 `(Turn, CallID)`；
//   - 没有可续的 → 明确报错（那一轮保持挂起，不收尾）。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/puras/mos"
	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/agent/policy"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// countingAnswerer 记录被问了几次（固定结果）。
type countingAnswerer struct {
	out   approval.Outcome
	calls int
}

func (a *countingAnswerer) Decide(ctx context.Context, req approval.Request) (approval.Outcome, error) {
	a.calls++
	return a.out, nil
}

// countingResumeTool 记录自己被执行了几次。
type countingResumeTool struct{ calls int }

func (c *countingResumeTool) Name() string        { return "stub" }
func (c *countingResumeTool) Description() string { return "计数工具" }
func (c *countingResumeTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (c *countingResumeTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	c.calls++
	return "ok", nil
}

// appendResumeEvent 往会话里追加一条事件。
func appendResumeEvent(t *testing.T, s *session.Session, ev session.Event) {
	t.Helper()
	if _, err := s.Append(ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// interruptedSession 造一个「被中断」的会话：turn 1 声明了调用但没有结果。
func interruptedSession(t *testing.T) *session.Session {
	t.Helper()
	sess := session.New("t", nil)
	appendResumeEvent(t, sess, session.Event{Kind: session.EventTurnStart, Turn: 1})
	appendResumeEvent(t, sess, session.Event{Kind: session.EventUser, Turn: 1, Text: "做一下"})
	appendResumeEvent(t, sess, session.Event{Kind: session.EventAssistant, Turn: 1, ToolCalls: []llm.ToolCall{
		{ID: "c1", Name: "stub", Args: "{}"},
	}})
	appendResumeEvent(t, sess, session.Event{Kind: session.EventToolCall, Turn: 1, ToolCall: &llm.ToolCall{
		ID: "c1", Name: "stub", Args: "{}",
	}})
	return sess
}

// 续跑：悬空调用走完 → 回填真实结果 → 继续问模型 → 本轮正常收尾。
func TestResumeContinuesInterruptedTurn(t *testing.T) {
	reg := tool.NewRegistry()
	ct := &countingResumeTool{}
	if err := reg.Register(ct); err != nil {
		t.Fatalf("注册: %v", err)
	}
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "续完了"}, Tools: reg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := interruptedSession(t)

	// 宿主先看清现场（派生读，不改日志）。
	hanging := sess.HangingCalls()
	if len(hanging) != 1 || hanging[0].CallID != "c1" {
		t.Fatalf("应报告 1 个悬空调用: %+v", hanging)
	}
	if turn := sess.InterruptedTurn(); turn != 1 {
		t.Fatalf("中断的轮次应是 1，got %d", turn)
	}

	reply, err := a.Resume(context.Background(), sess, mos.ResumeOptions{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if reply != "续完了" {
		t.Fatalf("reply = %q", reply)
	}
	if ct.calls != 1 {
		t.Fatalf("悬空调用应执行一次，got %d", ct.calls)
	}
	// 悬空调用清零。
	if n := len(sess.HangingCalls()); n != 0 {
		t.Fatalf("续跑后不应还有悬空调用，got %d", n)
	}
	// 本轮正常收尾，且沿用原轮次号。
	last := sess.Events()[len(sess.Events())-1]
	if last.Kind != session.EventTurnEnd || last.Reason != "completed" {
		t.Fatalf("续跑后应正常收尾: %+v", last)
	}
	if last.Turn != 1 {
		t.Fatalf("收尾轮次应沿用原轮次 1，got %d", last.Turn)
	}
}

// 没有可续的 → 明确报错，那一轮保持挂起（不收尾）。
func TestResumeWithoutHangingCallsReportsError(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := session.New("t", nil)
	appendResumeEvent(t, sess, session.Event{Kind: session.EventTurnStart, Turn: 1})

	if _, err := a.Resume(context.Background(), sess, mos.ResumeOptions{}); !errors.Is(err, react.ErrNothingToResume) {
		t.Fatalf("应报没有可续的，got %v", err)
	}
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventTurnEnd {
			t.Fatalf("没有可续的不该收尾: %+v", ev)
		}
	}
}

// 默认不续：不调 Resume，挂起的轮次就一直挂着（没人替宿主做主）。
func TestNoAutoResumeByDefault(t *testing.T) {
	reg := tool.NewRegistry()
	ct := &countingResumeTool{}
	if err := reg.Register(ct); err != nil {
		t.Fatalf("注册: %v", err)
	}
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}, Tools: reg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := interruptedSession(t)

	// 走一次普通 Run（**另起一轮**）：挂起的那一轮不受影响。
	if _, err := a.Run(context.Background(), sess, "新问题"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ct.calls != 0 {
		t.Fatalf("普通 Run 不该动悬空调用，got %d 次", ct.calls)
	}
	if n := len(sess.HangingCalls()); n != 1 {
		t.Fatalf("悬空调用应仍挂着，got %d", n)
	}
}

// 续跑时命中审批先验：同一轮同一次调用上已有人批过 → 不再问人。
func TestResumeReusesPriorApprovalDecision(t *testing.T) {
	reg := tool.NewRegistry()
	ct := &countingResumeTool{}
	if err := reg.Register(ct); err != nil {
		t.Fatalf("注册: %v", err)
	}
	// 应答方会拒绝：命中先验时它根本不该被问到。
	ans := &countingAnswerer{out: approval.Rejected}
	a, err := mos.New(mos.Config{
		Model: &stubModel{reply: "续完了"}, Tools: reg, Approval: ans,
		// 工具走「需人工确认」档（ask → 审批通道）。
		Rules: []policy.Check{func(ctx context.Context, call policy.Call) (policy.Verdict, string) {
			return policy.VerdictAsk, "这一步要人工确认"
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := interruptedSession(t)
	// 之前有人批过（同一轮、同一次调用）。
	appendResumeEvent(t, sess, session.Event{Kind: session.EventApprovalAsked, Turn: 1, Approval: &session.Approval{
		ID: "ap-old", ToolName: "stub", CallID: "c1", Reason: "要人工确认",
	}})
	appendResumeEvent(t, sess, session.Event{Kind: session.EventApprovalDecided, Turn: 1, Approval: &session.Approval{
		ID: "ap-old", ToolName: "stub", CallID: "c1", Outcome: "approved",
	}})

	if _, err := a.Resume(context.Background(), sess, mos.ResumeOptions{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if ans.calls != 0 {
		t.Fatalf("命中先验就不该问人，asked %d 次", ans.calls)
	}
	if ct.calls != 1 {
		t.Fatalf("复用批准后工具应执行，got %d", ct.calls)
	}
	// 审计：复用也留痕（新 id + reusedFrom）。
	var reused bool
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventApprovalAsked && ev.Approval != nil && ev.Approval.ReusedFrom == "ap-old" {
			reused = true
		}
	}
	if !reused {
		t.Fatalf("复用决定应留痕（reusedFrom=ap-old）: %+v", sess.Events())
	}
}
