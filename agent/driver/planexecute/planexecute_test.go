package planexecute

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// ---- mock 模型：离线演示用 ----

// planModel 返回固定计划文本（JSON 数组或行文本，由 steps 指定）。
type planModel struct {
	steps string
}

func (m *planModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *planModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: m.steps}}, nil
}
func (m *planModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// execModel 不调用工具，直接作答。
type execModel struct{ reply string }

func (m *execModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *execModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	reply := m.reply
	if reply == "" {
		reply = "完成"
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: reply}}, nil
}
func (m *execModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// failModel 执行阶段直接报错。
type failModel struct{}

func (m *failModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *failModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return nil, errors.New("mock model failed")
}
func (m *failModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// ---- 工具 ----

// noopTool 空工具（注册用，保证 Registry 非空）。
type noopTool struct{}

func (t noopTool) Name() string        { return "noop" }
func (t noopTool) Description() string { return "noop" }
func (t noopTool) Parameters() any     { return map[string]any{} }
func (t noopTool) Execute(context.Context, json.RawMessage) (any, error) {
	return "ok", nil
}

func newRT(plan, exec llm.ChatModel) *driver.RunContext {
	reg := tool.NewRegistry()
	_ = reg.Register(noopTool{})
	return &driver.RunContext{
		Tools:       reg,
		Planner:     planner.NewReAct(exec),
		PlanPlanner: planner.NewPlan(plan),
	}
}

func planEvents(sess *session.Session) []session.SessionEvent {
	var out []session.SessionEvent
	for _, ev := range sess.Events {
		if strings.HasPrefix(ev.Type, "plan/") {
			out = append(out, ev)
		}
	}
	return out
}

// TestPlanExecuteMultiStep 验证两步骤计划：计划产出 → 逐条执行 → completed。
func TestPlanExecuteMultiStep(t *testing.T) {
	rt := newRT(&planModel{steps: `[{"summary":"步骤一"},{"summary":"步骤二"}]`}, &execModel{})
	a := New(rt)
	sess := session.NewSession("s1")

	reply, err := a.Run(context.Background(), sess, "完成一个任务")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "完成" {
		t.Fatalf("reply = %q, want 完成", reply)
	}
	if sess.State.Status != session.StatusCompleted {
		t.Fatalf("status = %v, want completed", sess.State.Status)
	}
	if sess.State.Goal != "完成一个任务" {
		t.Fatalf("goal = %q, want 原始目标", sess.State.Goal)
	}

	// 事件序列：plan/start → plan/step(pending×2) → [running/done]×2 → plan/end
	evs := planEvents(sess)
	if len(evs) != 1+2+4+1 {
		t.Fatalf("plan events = %d, want 8: %+v", len(evs), evs)
	}
	if evs[0].Type != event.SessionEventPlanStart {
		t.Fatalf("first plan event = %s, want plan/start", evs[0].Type)
	}
	if evs[len(evs)-1].Type != event.SessionEventPlanEnd {
		t.Fatalf("last plan event = %s, want plan/end", evs[len(evs)-1].Type)
	}

	// Turn 全局递增：任务回合 1，步骤 1/2 分别 2/3。
	var turns []int
	for _, ev := range sess.Events {
		if ev.Type == event.SessionEventTurnStart {
			turns = append(turns, mustTurn(t, ev))
		}
	}
	if len(turns) != 3 || turns[0] != 1 || turns[1] != 2 || turns[2] != 3 {
		t.Fatalf("turn/starts = %v, want [1 2 3]", turns)
	}

	// 消息历史：目标 + 每步骤（user + assistant）
	if len(sess.Messages()) != 5 {
		t.Fatalf("messages = %d, want 5: %+v", len(sess.Messages()), sess.Messages())
	}
}

// TestPlanExecuteLineFallback 验证非 JSON 输出按行拆分回退。
func TestPlanExecuteLineFallback(t *testing.T) {
	rt := newRT(&planModel{steps: "1. 第一步\n2. 第二步\n3. 第三步"}, &execModel{})
	a := New(rt)
	sess := session.NewSession("s1")

	if _, err := a.Run(context.Background(), sess, "目标"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := planEvents(sess)
	if len(evs) != 1+3+6+1 {
		t.Fatalf("plan events = %d, want 11", len(evs))
	}
	if len(sess.Messages()) != 1+3*2 {
		t.Fatalf("messages = %d, want 7", len(sess.Messages()))
	}
}

// TestPlanExecuteNoPlanner 验证无规划器时退化为单步执行。
func TestPlanExecuteNoPlanner(t *testing.T) {
	rt := newRT(nil, &execModel{})
	rt.PlanPlanner = nil
	a := New(rt)
	sess := session.NewSession("s1")

	reply, err := a.Run(context.Background(), sess, "直接做")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "完成" {
		t.Fatalf("reply = %q", reply)
	}
	evs := planEvents(sess)
	if len(evs) != 5 {
		t.Fatalf("plan events = %d, want 5 (start + pending/running/done + end)", len(evs))
	}
}

// TestPlanExecuteVerifyFail 验证交付物校验失败 → blocked。
func TestPlanExecuteVerifyFail(t *testing.T) {
	rt := newRT(&planModel{steps: `[{"summary":"唯一一步"}]`}, &execModel{})
	a := New(rt, WithVerifier(func(context.Context, *driver.RunContext, planner.Plan, []planner.PlanStep) (bool, string) {
		return false, "交付物不达标"
	}))
	sess := session.NewSession("s1")

	if _, err := a.Run(context.Background(), sess, "目标"); err == nil {
		t.Fatal("Run should fail on verify")
	}
	if sess.State.Status != session.StatusBlocked {
		t.Fatalf("status = %v, want blocked", sess.State.Status)
	}
	evs := planEvents(sess)
	last := evs[len(evs)-1]
	if last.Type != event.SessionEventPlanEnd {
		t.Fatalf("last plan event = %s", last.Type)
	}
}

// TestPlanExecuteStepFailure 验证执行步骤失败 → blocked + plan/end failed。
func TestPlanExecuteStepFailure(t *testing.T) {
	rt := newRT(&planModel{steps: `[{"summary":"会失败的一步"}]`}, &failModel{})
	a := New(rt)
	sess := session.NewSession("s1")

	_, err := a.Run(context.Background(), sess, "目标")
	if err == nil || !strings.Contains(err.Error(), "plan step 1") {
		t.Fatalf("err = %v, want plan step 1 failure", err)
	}
	if sess.State.Status != session.StatusBlocked {
		t.Fatalf("status = %v, want blocked", sess.State.Status)
	}
	evs := planEvents(sess)
	last := evs[len(evs)-1]
	if last.Type != event.SessionEventPlanEnd {
		t.Fatalf("last plan event = %s", last.Type)
	}
}

func mustTurn(t *testing.T, ev session.SessionEvent) int {
	t.Helper()
	var d event.TurnData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("unmarshal turn/start: %v", err)
	}
	return d.Turn
}
