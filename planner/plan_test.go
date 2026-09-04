package planner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/puras/mos/llm"
)

// ---- mock 模型：离线演示用 ----

type planChatModel struct {
	content string
	err     error
}

func (m *planChatModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *planChatModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: m.content}}, nil
}
func (m *planChatModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

func TestParsePlanStepsJSON(t *testing.T) {
	steps := parsePlanSteps(`[{"summary":"调研"},{"summary":"实现"},{"summary":"验证"}]`)
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3: %+v", len(steps), steps)
	}
	if steps[0].Index != 1 || steps[1].Summary != "实现" {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestParsePlanStepsFencedJSON(t *testing.T) {
	steps := parsePlanSteps("```json\n[{\"summary\":\"一步\"}]\n```")
	if len(steps) != 1 || steps[0].Summary != "一步" {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestParsePlanStepsLines(t *testing.T) {
	steps := parsePlanSteps("1. 第一步\n- 第二步\n3）第三步\n第四步")
	if len(steps) != 4 {
		t.Fatalf("steps = %d, want 4: %+v", len(steps), steps)
	}
	for i, want := range []string{"第一步", "第二步", "第三步", "第四步"} {
		if steps[i].Summary != want {
			t.Fatalf("step %d = %q, want %q", i, steps[i].Summary, want)
		}
	}
}

func TestParsePlanStepsEmpty(t *testing.T) {
	if steps := parsePlanSteps("   \n\n"); len(steps) != 0 {
		t.Fatalf("steps = %+v, want empty", steps)
	}
}

func TestPlanModelError(t *testing.T) {
	p := NewPlan(&planChatModel{err: errors.New("boom")})
	if _, err := p.Plan(context.Background(), "目标", nil); err == nil {
		t.Fatal("Plan should fail on model error")
	}
}

func TestPlanFallbackSingleStep(t *testing.T) {
	p := NewPlan(&planChatModel{content: "   \n\n"})
	plan, err := p.Plan(context.Background(), "目标", nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Summary != "目标" {
		t.Fatalf("steps = %+v, want single-step fallback on empty output", plan.Steps)
	}
	if !strings.HasPrefix(plan.Goal, "目标") {
		t.Fatalf("goal = %q", plan.Goal)
	}
}
