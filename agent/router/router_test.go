package router

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/driver/planexecute"
	"hegq.net/mos/agent/driver/react"
	"hegq.net/mos/agent/driver/reflection"
	"hegq.net/mos/agent/driver/scripted"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// mockClassifier 固定返回驱动名（LLM 分类器替身）。
type mockClassifier struct {
	name string
	err  error
}

func (m *mockClassifier) Classify(context.Context, string, []llm.Message) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.name, nil
}

// genModel 直接作答（端到端运行验证用）。
type genModel struct{ reply string }

func (m *genModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *genModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	reply := m.reply
	if reply == "" {
		reply = "完成"
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: reply}}, nil
}
func (m *genModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// reviewModel 评审通过。
type reviewModel struct{}

func (m *reviewModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *reviewModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: `{"issues":[],"score":90}`}}, nil
}
func (m *reviewModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

type noopTool struct{}

func (t noopTool) Name() string        { return "noop" }
func (t noopTool) Description() string { return "noop" }
func (t noopTool) Parameters() any     { return map[string]any{} }
func (t noopTool) Execute(context.Context, json.RawMessage) (any, error) {
	return "ok", nil
}

func newBareRT(exec llm.ChatModel) *driver.RunContext {
	reg := tool.NewRegistry()
	_ = reg.Register(noopTool{})
	return &driver.RunContext{
		Tools:     reg,
		Planner:   planner.NewReAct(exec),
		Reflector: planner.NewReflector(&reviewModel{}),
	}
}

// TestRuleHardRoute 验证"先规划"显式指令规则硬路由 → planexecute。
func TestRuleHardRoute(t *testing.T) {
	r := NewHybrid()
	d, err := r.Select(context.Background(), "先规划这个多步任务", session.NewSession("s1"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, ok := d.(*planexecute.PlanExecuteDriver); !ok {
		t.Fatalf("driver = %T, want *planexecute.PlanExecuteDriver", d)
	}
}

// TestFallbackReact 验证未命中规则且无分类器时走默认兜底 react。
func TestFallbackReact(t *testing.T) {
	r := NewHybrid()
	d, err := r.Select(context.Background(), "现在几点了", session.NewSession("s1"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, ok := d.(*react.ReActDriver); !ok {
		t.Fatalf("driver = %T, want *react.ReActDriver", d)
	}
}

// TestClassifierRoute 验证规则未命中时 LLM 分类器兜底。
func TestClassifierRoute(t *testing.T) {
	r := NewHybrid(WithClassifier(&mockClassifier{name: planner.DriverPlanExec}))
	d, err := r.Select(context.Background(), "帮我整理一份发布方案", session.NewSession("s1"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, ok := d.(*planexecute.PlanExecuteDriver); !ok {
		t.Fatalf("driver = %T, want *planexecute.PlanExecuteDriver", d)
	}
}

// TestClassifierReflection 验证分类为 reflection。
func TestClassifierReflection(t *testing.T) {
	r := NewHybrid(WithClassifier(&mockClassifier{name: planner.DriverReflection}))
	d, err := r.Select(context.Background(), "写一段宣传文案", session.NewSession("s1"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, ok := d.(*reflection.ReflectionDriver); !ok {
		t.Fatalf("driver = %T, want *reflection.ReflectionDriver", d)
	}
}

// TestClassifierFailure 验证分类器出错时静默走 fallback。
func TestClassifierFailure(t *testing.T) {
	r := NewHybrid(WithClassifier(&mockClassifier{err: errors.New("boom")}))
	d, err := r.Select(context.Background(), "随便什么", session.NewSession("s1"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, ok := d.(*react.ReActDriver); !ok {
		t.Fatalf("driver = %T, want fallback react", d)
	}
}

// TestUnknownFallback 验证未知驱动名返回错误。
func TestUnknownFallback(t *testing.T) {
	r := NewHybrid(WithFallback("nope"))
	if _, err := r.Select(context.Background(), "x", session.NewSession("s1")); err == nil {
		t.Fatal("Select should fail on unknown fallback driver")
	}
}

// TestHybridRouterScriptedRule 验证 scripted 工厂已在 NewHybrid 内置注册；
// 显式加规则后，关键词命中返回 ScriptedDriver。
func TestHybridRouterScriptedRule(t *testing.T) {
	r := NewHybrid(
		WithRule("批量", planner.DriverScripted),
	)
	d, err := r.Select(context.Background(), "批量重命名文件", session.NewSession("s"))
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if _, ok := d.(*scripted.ScriptedDriver); !ok {
		t.Fatalf("expected *scripted.ScriptedDriver, got %T", d)
	}
}

// TestEndToEndBareDriver 验证 Router 返回的裸驱动 + 裸 RunContext 可运行
// （ReActDriver 惰性自装配默认钩子/守卫）。
func TestEndToEndBareDriver(t *testing.T) {
	rt := newBareRT(&genModel{})
	r := NewHybrid()
	d, err := r.Select(context.Background(), "完成一个简单任务", session.NewSession("s1"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	ag := driver.New(rt, d)
	sess := session.NewSession("s1")
	if _, err := ag.Run(context.Background(), sess, "完成一个简单任务"); err != nil {
		t.Fatalf("Run (bare react): %v", err)
	}

	// planexecute 裸驱动：单步退化 + ReAct 惰性装配
	rt2 := newBareRT(&genModel{})
	d2, err := NewHybrid().Select(context.Background(), "先规划任务", session.NewSession("s1"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, err := driver.New(rt2, d2).Run(context.Background(), session.NewSession("s2"), "先规划任务"); err != nil {
		t.Fatalf("Run (bare planexecute): %v", err)
	}

	// reflection 裸驱动：reflector 已注入 rt
	rt3 := newBareRT(&genModel{})
	d3, err := NewHybrid(WithClassifier(&mockClassifier{name: planner.DriverReflection})).Select(
		context.Background(), "写一段文案", session.NewSession("s1"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, err := driver.New(rt3, d3).Run(context.Background(), session.NewSession("s3"), "写一段文案"); err != nil {
		t.Fatalf("Run (bare reflection): %v", err)
	}
}
