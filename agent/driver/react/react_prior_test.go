package react

// 先验决定复用的驱动侧接线（FR-G3 / S9.1）：
// 命中 → 不问人，直接复用；审计记新 id + reusedFrom。

import (
	"context"
	"testing"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// 先验命中 → 不问人，工具照常执行。
func TestPriorHitSkipsAsking(t *testing.T) {
	sess := session.New("t", nil)
	ct := &countingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(ct); err != nil {
		t.Fatalf("注册: %v", err)
	}
	// 应答方会拒绝：命中先验时它根本不该被问到。
	ans := &scriptedAnswerer{out: approval.Rejected}
	p := &scriptedPlanner{acts: []planner.Action{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "danger", Args: "{}"}}, FinishReason: "tool_use"},
		{Content: "好了"},
	}}
	d := driver.New(&driver.RunContext{
		Planner:  p,
		Tools:    reg,
		Hooks:    askHook("这一步要人工确认"),
		Answerer: ans,
		PriorApproval: func(ctx context.Context, s *session.Session, req approval.Request) (approval.PriorDecision, bool) {
			return approval.PriorDecision{Outcome: approval.Approved, ReusedFrom: "ap-old"}, true
		},
	}, New())

	if _, err := d.Run(context.Background(), sess, "做一下"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ct.calls != 1 {
		t.Fatalf("复用批准后工具应执行，calls = %d", ct.calls)
	}
	if len(ans.seen) != 0 {
		t.Fatalf("命中先验就不该问人，asked %d 次", len(ans.seen))
	}

	// 审计：asked + decided 一对，新 id，载荷带 reusedFrom。
	var asked, decided *session.Approval
	for _, ev := range sess.Events() {
		if ev.Approval == nil {
			continue
		}
		switch ev.Kind {
		case session.EventApprovalAsked:
			asked = ev.Approval
		case session.EventApprovalDecided:
			decided = ev.Approval
		}
	}
	if asked == nil || decided == nil {
		t.Fatal("复用也要记审计（asked + decided 一对）")
	}
	if asked.ID == "" || asked.ID == "ap-old" {
		t.Fatalf("复用应记新 id，got %q", asked.ID)
	}
	if asked.ReusedFrom != "ap-old" || decided.ReusedFrom != "ap-old" {
		t.Fatalf("审计应指回被复用的决定: asked=%+v decided=%+v", asked, decided)
	}
	if decided.Outcome != "approved" {
		t.Fatalf("复用结果 = %q, want approved", decided.Outcome)
	}
}

// 先验没命中 → 照常问人（应答方的结果生效）。
func TestPriorMissStillAsks(t *testing.T) {
	sess := session.New("t", nil)
	ct := &countingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(ct); err != nil {
		t.Fatalf("注册: %v", err)
	}
	ans := &scriptedAnswerer{out: approval.Approved}
	p := &scriptedPlanner{acts: []planner.Action{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "danger", Args: "{}"}}, FinishReason: "tool_use"},
		{Content: "好了"},
	}}
	d := driver.New(&driver.RunContext{
		Planner:  p,
		Tools:    reg,
		Hooks:    askHook("这一步要人工确认"),
		Answerer: ans,
		PriorApproval: func(ctx context.Context, s *session.Session, req approval.Request) (approval.PriorDecision, bool) {
			return approval.PriorDecision{}, false
		},
	}, New())

	if _, err := d.Run(context.Background(), sess, "做一下"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ans.seen) != 1 {
		t.Fatalf("没命中就该问人，asked %d 次", len(ans.seen))
	}
	if ct.calls != 1 {
		t.Fatalf("批准后工具应执行，calls = %d", ct.calls)
	}
}
