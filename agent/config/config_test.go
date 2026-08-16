package config

import (
	"context"
	"strings"
	"testing"

	"hegq.net/mos/agent/seam"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
)

// ---- mock 模型：离线演示用 ----

type mockModel struct {
	content string
}

func (m *mockModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *mockModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	reply := m.content
	if reply == "" {
		reply = "完成"
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: reply}}, nil
}
func (m *mockModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

func newRegistry(model llm.ChatModel) *seam.SeamRegistry {
	reg := seam.New()
	reg.Register(seam.KindModel, "mock", func() any { return model })
	return reg
}

// TestLoadDefaultReAct 验证缺省配置：react 驱动 + 内置工具 + default 策略。
func TestLoadDefaultReAct(t *testing.T) {
	reg := newRegistry(&mockModel{})
	a, err := Assemble(reg, []byte("model: mock\n"))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sess := a.NewSession()
	reply, err := a.Run(context.Background(), sess, "你好")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "完成" {
		t.Fatalf("reply = %q", reply)
	}
}

// TestAssemblePlanExecute 验证 Driver=plan-execute + plan_planner 按名解析。
func TestAssemblePlanExecute(t *testing.T) {
	reg := newRegistry(&mockModel{})
	reg.Register(seam.KindPlanner, "myplan", func() any {
		return planner.NewPlan(&mockModel{content: `[{"summary":"一步"}]`})
	})
	a, err := Assemble(reg, []byte(`
model: mock
driver: plan-execute
plan_planner: myplan
`))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	reply, err := a.Run(context.Background(), a.NewSession(), "做个任务")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "完成" {
		t.Fatalf("reply = %q", reply)
	}
}

// TestAssembleReflection 验证 Driver=reflection + reflector 按名解析。
func TestAssembleReflection(t *testing.T) {
	reg := newRegistry(&mockModel{})
	reg.Register(seam.KindReflector, "myreview", func() any {
		return planner.NewReflector(&mockModel{content: `{"issues":[],"score":90}`})
	})
	a, err := Assemble(reg, []byte(`
model: mock
driver: reflection
reflector: myreview
`))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	reply, err := a.Run(context.Background(), a.NewSession(), "写段文案")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "完成" {
		t.Fatalf("reply = %q", reply)
	}
}

// TestAssembleMissingModel 验证缺少模型 fail-fast。
func TestAssembleMissingModel(t *testing.T) {
	reg := seam.New()
	if _, err := Assemble(reg, []byte("driver: react\n")); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("err = %v, want model required", err)
	}
}

// TestAssembleUnknownDriver 验证未知驱动名 fail-fast。
func TestAssembleUnknownDriver(t *testing.T) {
	reg := newRegistry(&mockModel{})
	if _, err := Assemble(reg, []byte("model: mock\ndriver: graph\n")); err == nil || !strings.Contains(err.Error(), "unknown driver") {
		t.Fatalf("err = %v, want unknown driver", err)
	}
}

// TestAssembleWrongModelType 验证 model provider 类型不匹配 fail-fast。
func TestAssembleWrongModelType(t *testing.T) {
	reg := seam.New()
	reg.Register(seam.KindModel, "mock", func() any { return "not a model" })
	if _, err := Assemble(reg, []byte("model: mock\n")); err == nil || !strings.Contains(err.Error(), "not llm.ChatModel") {
		t.Fatalf("err = %v, want type mismatch", err)
	}
}

// TestAssembleMissingPlanPlanner 验证 plan-execute 无规划器时单步退化（不报错）。
func TestAssembleMissingPlanPlanner(t *testing.T) {
	reg := newRegistry(&mockModel{})
	a, err := Assemble(reg, []byte("model: mock\ndriver: plan-execute\n"))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if _, err := a.Run(context.Background(), a.NewSession(), "任务"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestLoadInvalidYAML 验证非法 YAML 报错。
func TestLoadInvalidYAML(t *testing.T) {
	if _, err := Load([]byte("model: [unclosed")); err == nil {
		t.Fatal("Load should fail on invalid yaml")
	}
}
