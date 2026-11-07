package session

import (
	"testing"

	"github.com/puras/mos/llm"
)

// askEvent 造一条「问了人工」事件。
func askEvent(turn int, id, callID, tool string) Event {
	return Event{Kind: EventApprovalAsked, Turn: turn, Approval: &Approval{
		ID: id, ToolName: tool, CallID: callID, Reason: "要人工确认",
	}}
}

// decideEvent 造一条「人工答了」事件。
func decideEvent(turn int, id, callID, tool, outcome string) Event {
	return Event{Kind: EventApprovalDecided, Turn: turn, Approval: &Approval{
		ID: id, ToolName: tool, CallID: callID, Outcome: outcome,
	}}
}

// 未决审批 = 有 asked 无 decided（按 id 配对）。
func TestPendingApprovalsDerivedFromLog(t *testing.T) {
	s := New("t", nil)
	mustAppend(t, s, askEvent(1, "a1", "c1", "shell"))
	mustAppend(t, s, decideEvent(1, "a1", "c1", "shell", "approved"))
	mustAppend(t, s, askEvent(1, "a2", "c2", "write"))

	got := s.PendingApprovals()
	if len(got) != 1 {
		t.Fatalf("未决审批应只剩 1 个，got %d: %+v", len(got), got)
	}
	if got[0].ID != "a2" || got[0].CallID != "c2" || got[0].ToolName != "write" {
		t.Fatalf("未决审批内容不对: %+v", got[0])
	}
	if got[0].Outcome != "" {
		t.Fatalf("未决审批不该带结果: %+v", got[0])
	}
}

// 配对不串：不同 id 的决定不能把别人的 asked 消掉。
func TestPendingApprovalsPairingDoesNotCross(t *testing.T) {
	s := New("t", nil)
	mustAppend(t, s, askEvent(1, "a1", "c1", "shell"))
	mustAppend(t, s, askEvent(1, "a2", "c2", "write"))
	mustAppend(t, s, decideEvent(1, "a2", "c2", "write", "rejected"))

	got := s.PendingApprovals()
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("只有 a2 被决定，a1 仍未决，got %+v", got)
	}
}

// 已决结果按 id 查。
func TestApprovalOutcomeByID(t *testing.T) {
	s := New("t", nil)
	mustAppend(t, s, askEvent(1, "a1", "c1", "shell"))
	mustAppend(t, s, decideEvent(1, "a1", "c1", "shell", "rejected"))

	out, ok := s.ApprovalOutcome("a1")
	if !ok || out != "rejected" {
		t.Fatalf("a1 的结果应是 rejected，got %q ok=%v", out, ok)
	}
	if _, ok := s.ApprovalOutcome("a2"); ok {
		t.Fatal("不存在的 id 不该有结果")
	}
	if _, ok := s.ApprovalOutcome(""); ok {
		t.Fatal("空 id 不该有结果")
	}
}

// 先验查询的匹配键是 (Turn, CallID)，不是 id。
func TestApprovalDecisionMatchesTurnAndCallID(t *testing.T) {
	s := New("t", nil)
	mustAppend(t, s, askEvent(3, "a1", "c1", "shell"))
	mustAppend(t, s, decideEvent(3, "a1", "c1", "shell", "approved"))

	d, ok := s.ApprovalDecision(3, "c1")
	if !ok {
		t.Fatal("同轮同调用应命中已决记录")
	}
	if d.ID != "a1" || d.Outcome != "approved" {
		t.Fatalf("命中的决定不对: %+v", d)
	}
	if _, ok := s.ApprovalDecision(4, "c1"); ok {
		t.Fatal("轮次不同不该命中")
	}
	if _, ok := s.ApprovalDecision(3, "c2"); ok {
		t.Fatal("调用不同不该命中")
	}
	if _, ok := s.ApprovalDecision(3, ""); ok {
		t.Fatal("空 callID 不该命中")
	}
}

// 悬空调用 = 有 tool/call 无 tool/result。
func TestHangingCallsDerivedFromLog(t *testing.T) {
	s := New("t", nil)
	mustAppend(t, s, Event{Kind: EventToolCall, Turn: 2, ToolCall: &llm.ToolCall{
		ID: "c1", Name: "shell", Args: `{"cmd":"ls"}`,
	}})
	mustAppend(t, s, Event{Kind: EventToolResult, Turn: 2, ToolResult: &ToolResult{
		CallID: "c1", Content: "ok",
	}})
	mustAppend(t, s, Event{Kind: EventToolCall, Turn: 2, ToolCall: &llm.ToolCall{
		ID: "c2", Name: "write", Args: `{"path":"a"}`,
	}})

	got := s.HangingCalls()
	if len(got) != 1 {
		t.Fatalf("悬空调用应只剩 1 个，got %d: %+v", len(got), got)
	}
	h := got[0]
	if h.CallID != "c2" || h.ToolName != "write" || h.Turn != 2 {
		t.Fatalf("悬空调用内容不对: %+v", h)
	}
	if h.Args != `{"path":"a"}` || h.Seq == 0 {
		t.Fatalf("悬空调用应带参数与序号: %+v", h)
	}
}

// 悬空调用与未决审批能对上（同一个 CallID）。
func TestPendingApprovalMatchesHangingCall(t *testing.T) {
	s := New("t", nil)
	mustAppend(t, s, Event{Kind: EventToolCall, Turn: 1, ToolCall: &llm.ToolCall{
		ID: "c1", Name: "danger", Args: "{}",
	}})
	mustAppend(t, s, askEvent(1, "a1", "c1", "danger"))

	pending := s.PendingApprovals()
	hanging := s.HangingCalls()
	if len(pending) != 1 || len(hanging) != 1 {
		t.Fatalf("未决审批 %d 个、悬空调用 %d 个，应各 1 个", len(pending), len(hanging))
	}
	if pending[0].CallID != hanging[0].CallID {
		t.Fatalf("对不上: 审批 %q vs 调用 %q", pending[0].CallID, hanging[0].CallID)
	}
	if pending[0].ToolName != hanging[0].ToolName {
		t.Fatalf("工具名对不上: %q vs %q", pending[0].ToolName, hanging[0].ToolName)
	}
}

// 派生是纯读：不写日志、不改状态。
func TestDerivedReadsDoNotMutateLog(t *testing.T) {
	s := New("t", nil)
	mustAppend(t, s, askEvent(1, "a1", "c1", "shell"))
	before := len(s.Events())

	s.PendingApprovals()
	s.ApprovalOutcome("a1")
	s.ApprovalDecision(1, "c1")
	s.HangingCalls()

	if after := len(s.Events()); after != before {
		t.Fatalf("派生读不该写日志：before=%d after=%d", before, after)
	}
}
