package react

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// countingTool 记录自己被执行了几次。
type countingTool struct{ calls int }

func (c *countingTool) Name() string        { return "danger" }
func (c *countingTool) Description() string { return "需要审批的工具" }
func (c *countingTool) Parameters() any     { return &struct{}{} }
func (c *countingTool) Execute(ctx context.Context, _ json.RawMessage) (any, error) {
	c.calls++
	return map[string]any{"ok": true}, nil
}

// scriptedAnswerer 按预设结果回答审批请求。
type scriptedAnswerer struct {
	out  approval.Outcome
	seen []approval.Request
}

func (a *scriptedAnswerer) Decide(ctx context.Context, req approval.Request) (approval.Outcome, error) {
	a.seen = append(a.seen, req)
	return a.out, nil
}

// askHook 让每次 pre-execute 都请求人工确认。
func askHook(reason string) *hook.HookBus {
	hb := hook.NewHookBus()
	hb.On(hook.HookToolPreExecute, func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		ev.Decision = hook.Decision{Kind: hook.DecisionAsk, Reason: reason}
		return next(ctx, ev)
	})
	return hb
}

// 审批通过 → 工具执行，审计两条事件配对。
func TestApprovalGrantedExecutesTool(t *testing.T) {
	sess := session.New("t", nil)
	ct := &countingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(ct); err != nil {
		t.Fatalf("注册: %v", err)
	}
	ans := &scriptedAnswerer{out: approval.Approved}
	p := &scriptedPlanner{acts: []planner.Action{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "danger", Args: "{}"}}},
		{Content: "好了"},
	}}
	d := driver.New(&driver.RunContext{
		Planner:  p,
		Tools:    reg,
		Hooks:    askHook("这一步要人工确认"),
		Answerer: ans,
	}, New())

	if _, err := d.Run(context.Background(), sess, "做一下"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ct.calls != 1 {
		t.Fatalf("批准后工具应执行，calls = %d", ct.calls)
	}
	if len(ans.seen) != 1 {
		t.Fatalf("应问过一次，got %d", len(ans.seen))
	}
	if ans.seen[0].ToolName != "danger" || ans.seen[0].Reason != "这一步要人工确认" {
		t.Fatalf("审批请求内容不对: %+v", ans.seen[0])
	}

	// 审计：asked 与 decided 两条，同一个 id 配对
	var asked, decided *session.Approval
	for _, ev := range sess.Events() {
		switch ev.Kind {
		case session.EventApprovalAsked:
			asked = ev.Approval
		case session.EventApprovalDecided:
			decided = ev.Approval
		}
	}
	if asked == nil || decided == nil {
		t.Fatalf("应落审批审计事件: %+v", sess.Events())
	}
	if asked.ID == "" || asked.ID != decided.ID {
		t.Fatalf("asked/decided 应同一 id: %q vs %q", asked.ID, decided.ID)
	}
	if decided.Outcome != "approved" {
		t.Fatalf("decided outcome = %q", decided.Outcome)
	}
	if asked.ToolName != "danger" || asked.CallID != "c1" {
		t.Fatalf("审计内容不对: %+v", asked)
	}
}

// 审批被拒 → 工具不执行，理由回填给模型。
func TestApprovalRejectedBlocksTool(t *testing.T) {
	sess := session.New("t", nil)
	ct := &countingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(ct); err != nil {
		t.Fatalf("注册: %v", err)
	}
	ans := &scriptedAnswerer{out: approval.Rejected}
	p := &scriptedPlanner{acts: []planner.Action{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "danger", Args: "{}"}}},
		{Content: "好"},
	}}
	d := driver.New(&driver.RunContext{
		Planner: p, Tools: reg, Hooks: askHook("这一步要人工确认"), Answerer: ans,
	}, New())

	if _, err := d.Run(context.Background(), sess, "做一下"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ct.calls != 0 {
		t.Fatalf("被拒的工具不该执行，calls = %d", ct.calls)
	}
	var result string
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult && ev.ToolResult != nil {
			result = ev.ToolResult.Content
		}
	}
	// 拒绝文案要带"不许绕"的语义（对齐 DSH："stop and explain instead of
	// working around it"）——被拒后模型该停下解释，不是换条路再试。
	if !strings.Contains(result, "保持拒绝状态") || !strings.Contains(result, "不要尝试用其他命令或方式") {
		t.Fatalf("拒绝理由应含制止绕过的语义: %s", result)
	}
}

// 没装配应答方 → ask 降级为拒绝（fail-closed），工具不执行。
func TestApprovalNotConfiguredFailsClosed(t *testing.T) {
	sess := session.New("t", nil)
	ct := &countingTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(ct); err != nil {
		t.Fatalf("注册: %v", err)
	}
	p := &scriptedPlanner{acts: []planner.Action{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "danger", Args: "{}"}}},
		{Content: "好"},
	}}
	d := driver.New(&driver.RunContext{
		Planner: p, Tools: reg, Hooks: askHook("这一步要人工确认"),
	}, New())

	if _, err := d.Run(context.Background(), sess, "做一下"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ct.calls != 0 {
		t.Fatalf("没装配审批通道时不该执行，calls = %d", ct.calls)
	}
	var result string
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventToolResult && ev.ToolResult != nil {
			result = ev.ToolResult.Content
		}
	}
	if !strings.Contains(result, "未装配审批通道") {
		t.Fatalf("应说明未装配审批通道: %s", result)
	}
	// 没有审批通道就不该有审批审计事件
	for _, ev := range sess.Events() {
		if ev.Kind == session.EventApprovalAsked {
			t.Fatalf("没装配通道时不该有审批事件: %+v", ev)
		}
	}
}
