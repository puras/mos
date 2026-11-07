package mos

// 装配层胶水的测试（内部测试包：approvalPrior 是装配层的私有函数）。

import (
	"context"
	"testing"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
)

func appendEvent(t *testing.T, s *session.Session, ev session.Event) {
	t.Helper()
	if _, err := s.Append(ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// 悬空调用上的已决决定 → 命中（复用，不再问人）。
func TestApprovalPriorMatchesHangingCall(t *testing.T) {
	sess := session.New("t", nil)
	appendEvent(t, sess, session.Event{Kind: session.EventToolCall, Turn: 3, ToolCall: &llm.ToolCall{
		ID: "c1", Name: "danger", Args: "{}",
	}})
	appendEvent(t, sess, session.Event{Kind: session.EventApprovalAsked, Turn: 3, Approval: &session.Approval{
		ID: "ap-old", ToolName: "danger", CallID: "c1",
	}})
	appendEvent(t, sess, session.Event{Kind: session.EventApprovalDecided, Turn: 3, Approval: &session.Approval{
		ID: "ap-old", ToolName: "danger", CallID: "c1", Outcome: "approved",
	}})

	d, ok := approvalPrior(sess)(context.Background(), sess, approval.Request{
		ToolName: "danger", CallID: "c1", Turn: 3,
	})
	if !ok {
		t.Fatal("悬空调用上的已决决定应命中")
	}
	if d.Outcome != approval.Approved {
		t.Fatalf("outcome = %v, want Approved", d.Outcome)
	}
	if d.ReusedFrom != "ap-old" {
		t.Fatalf("reusedFrom = %q, want ap-old", d.ReusedFrom)
	}
}

// 已经跑完的调用不借用决定：「批准只对本次调用生效」。
func TestApprovalPriorSkipsFinishedCall(t *testing.T) {
	sess := session.New("t", nil)
	appendEvent(t, sess, session.Event{Kind: session.EventToolCall, Turn: 3, ToolCall: &llm.ToolCall{
		ID: "c1", Name: "danger", Args: "{}",
	}})
	appendEvent(t, sess, session.Event{Kind: session.EventToolResult, Turn: 3, ToolResult: &session.ToolResult{
		CallID: "c1", Content: "done",
	}})
	appendEvent(t, sess, session.Event{Kind: session.EventApprovalAsked, Turn: 3, Approval: &session.Approval{
		ID: "ap-old", ToolName: "danger", CallID: "c1",
	}})
	appendEvent(t, sess, session.Event{Kind: session.EventApprovalDecided, Turn: 3, Approval: &session.Approval{
		ID: "ap-old", ToolName: "danger", CallID: "c1", Outcome: "approved",
	}})

	if _, ok := approvalPrior(sess)(context.Background(), sess, approval.Request{
		ToolName: "danger", CallID: "c1", Turn: 3,
	}); ok {
		t.Fatal("已跑完的调用不该借用决定")
	}
}

// 匹配键是 (Turn, CallID)：轮次或调用不同都不命中。
func TestApprovalPriorRequiresSameTurnAndCall(t *testing.T) {
	sess := session.New("t", nil)
	appendEvent(t, sess, session.Event{Kind: session.EventToolCall, Turn: 3, ToolCall: &llm.ToolCall{
		ID: "c1", Name: "danger", Args: "{}",
	}})
	appendEvent(t, sess, session.Event{Kind: session.EventApprovalDecided, Turn: 3, Approval: &session.Approval{
		ID: "ap-old", ToolName: "danger", CallID: "c1", Outcome: "approved",
	}})

	p := approvalPrior(sess)
	if _, ok := p(context.Background(), sess, approval.Request{ToolName: "danger", CallID: "c1", Turn: 4}); ok {
		t.Fatal("轮次不同不该命中")
	}
	if _, ok := p(context.Background(), sess, approval.Request{ToolName: "danger", CallID: "c2", Turn: 3}); ok {
		t.Fatal("调用不同不该命中")
	}
	if _, ok := p(context.Background(), sess, approval.Request{ToolName: "write", CallID: "c1", Turn: 3}); ok {
		t.Fatal("工具不同不该命中")
	}
}

// 没有决定 → 不命中（该问还是要问）。
func TestApprovalPriorMissWithoutDecision(t *testing.T) {
	sess := session.New("t", nil)
	appendEvent(t, sess, session.Event{Kind: session.EventToolCall, Turn: 3, ToolCall: &llm.ToolCall{
		ID: "c1", Name: "danger", Args: "{}",
	}})

	if _, ok := approvalPrior(sess)(context.Background(), sess, approval.Request{
		ToolName: "danger", CallID: "c1", Turn: 3,
	}); ok {
		t.Fatal("没有已决决定就不该命中")
	}
}
