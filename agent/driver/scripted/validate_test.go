package scripted

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/puras/mos/planner"
	"github.com/puras/mos/tool"
)

// stubTool 是 validate_test 用的最小工具：Parameters() 返回带 properties 的 schema，
// 以驱动参数名校验；Execute 永远成功。
type stubTool struct{ name, desc string }

func (t stubTool) Name() string        { return t.name }
func (t stubTool) Description() string { return t.desc }
func (t stubTool) Parameters() any {
	return struct {
		Path string `json:"path"`
	}{}
}
func (t stubTool) Execute(_ context.Context, _ json.RawMessage) (any, error) { return "ok", nil }

// reg 是 validate_test 的工具注册助手（注册 stubTool，名称任意）。
func reg(t *testing.T, names ...string) *tool.Registry {
	t.Helper()
	r := tool.NewRegistry()
	for _, n := range names {
		if err := r.Register(stubTool{name: n, desc: n}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
	return r
}

// step 是 ScriptStep 的构造助手（仅 ID + 工具名）。
func step(id, toolName string) planner.ScriptStep {
	return planner.ScriptStep{ID: id, Tool: toolName}
}

// TestValidateRejectsUnknownTool 验证未知工具名 → 校验拒绝。
func TestValidateRejectsUnknownTool(t *testing.T) {
	r := reg(t, "noop")
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{step("a", "nope")}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

// TestValidateRejectsCycle 验证依赖环 → 校验拒绝（Kahn 拓扑排序检测）。
func TestValidateRejectsCycle(t *testing.T) {
	r := reg(t, "noop")
	a, b := step("a", "noop"), step("b", "noop")
	a.DependsOn = []string{"b"}
	b.DependsOn = []string{"a"}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{a, b}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for cycle")
	}
}

// TestValidateRejectsUnknownDependency 验证 DependsOn 指向不存在的步骤 → 校验拒绝。
func TestValidateRejectsUnknownDependency(t *testing.T) {
	r := reg(t, "noop")
	a := step("a", "noop")
	a.DependsOn = []string{"ghost"}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{a}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for unknown dependency")
	}
}

// TestValidateRejectsDupIDAndSelfDep 验证重复 ID 与自依赖：两条独立校验路径。
func TestValidateRejectsDupIDAndSelfDep(t *testing.T) {
	r := reg(t, "noop")
	a := step("a", "noop")
	a.DependsOn = []string{"a"}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{a}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for self-dependency")
	}
	plan = &planner.ScriptedPlan{Steps: []planner.ScriptStep{step("a", "noop"), step("a", "noop")}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for duplicate id")
	}
}

// TestValidateRejectsMaxSteps 验证步数超上限 → 校验拒绝。
func TestValidateRejectsMaxSteps(t *testing.T) {
	r := reg(t, "noop")
	steps := make([]planner.ScriptStep, 3)
	for i := range steps {
		steps[i] = step(string(rune('a'+i)), "noop")
	}
	plan := &planner.ScriptedPlan{Steps: steps}
	if err := validateScript(plan, r, 2); err == nil {
		t.Fatal("expected error exceeding maxSteps")
	}
}

// TestValidateRejectsUnknownArgName 验证 Args 中的键不在 schema properties 内 → 校验拒绝。
func TestValidateRejectsUnknownArgName(t *testing.T) {
	r := reg(t, "noop")
	s := step("a", "noop")
	s.Args = map[string]any{"nope_field": 1}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{s}}
	if err := validateScript(plan, r, 16); err == nil {
		t.Fatal("expected error for unknown arg name")
	}
}

// TestValidateAcceptsValid 验证合法脚本 → 校验通过：依赖合法 + 参数键集 ⊆ properties。
func TestValidateAcceptsValid(t *testing.T) {
	r := reg(t, "noop")
	a, b := step("a", "noop"), step("b", "noop")
	b.DependsOn = []string{"a"}
	b.Args = map[string]any{"path": "/x"}
	plan := &planner.ScriptedPlan{Steps: []planner.ScriptStep{a, b}}
	if err := validateScript(plan, r, 16); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
}
