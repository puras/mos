package prompt

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/puras/mos/tool"
)

// guidedTool 实现 tool.Guideline 的测试工具；plainTool 不实现。
type guidedTool struct{ name, text string }

func (g guidedTool) Name() string        { return g.name }
func (g guidedTool) Description() string { return "测试工具" }
func (g guidedTool) Parameters() any     { return &struct{}{} }
func (g guidedTool) Guideline() string   { return g.text }
func (g guidedTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return nil, nil
}

type plainTool struct{ name string }

func (p plainTool) Name() string        { return p.name }
func (p plainTool) Description() string { return "无指引的测试工具" }
func (p plainTool) Parameters() any     { return &struct{}{} }
func (p plainTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return nil, nil
}

func TestRenderOrdersDeterministically(t *testing.T) {
	b := New()
	// 故意乱序加入；空文本节应被剔除；同序按名字字典序。
	sections := []Section{
		{Name: "late", Order: OrderRuntime, Text: "晚"},
		{Name: "tool:b", Order: OrderToolBase + 1, Text: "乙"},
		{Name: "empty", Order: 1, Text: "   "},
		{Name: "tool:a", Order: OrderToolBase, Text: "甲"},
		{Name: "first", Order: OrderIdentity, Text: "先"},
		{Name: "tie-b", Order: 42, Text: "同序乙"},
		{Name: "tie-a", Order: 42, Text: "同序甲"},
	}
	for _, s := range sections {
		if err := b.Add(s); err != nil {
			t.Fatalf("Add(%s): %v", s.Name, err)
		}
	}
	want := strings.Join([]string{"先", "同序甲", "同序乙", "甲", "乙", "晚"}, "\n\n")
	// 渲染可重入：两次结果恒等（顺序稳定性利于 prompt cache）。
	if got := b.Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
	if again := b.Render(); again != want {
		t.Fatalf("Render 两次结果不一致（确定性被破坏）")
	}
}

func TestAddRejectsDuplicateAndEmpty(t *testing.T) {
	b := New()
	if err := b.Add(Section{Name: "x", Text: "1"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := b.Add(Section{Name: "x", Text: "2"}); err == nil {
		t.Fatal("重名 Add 应报错（fail-closed，不静默覆盖）")
	}
	if err := b.Add(Section{Name: "", Text: "3"}); err == nil {
		t.Fatal("空名 Add 应报错")
	}
	// 报错不得污染已有分节
	if got := b.Render(); got != "1" {
		t.Fatalf("Render = %q, want %q", got, "1")
	}
}

func TestSetUpserts(t *testing.T) {
	b := Default()
	// 覆写默认身份节
	if err := b.Set(Section{Name: "identity", Order: OrderIdentity, Text: "自定义身份"}); err != nil {
		t.Fatalf("Set 覆写: %v", err)
	}
	// 新名字等同 Add
	if err := b.Set(Section{Name: "persona", Order: OrderPersona, Text: "人格"}); err != nil {
		t.Fatalf("Set 新增: %v", err)
	}
	want := "自定义身份\n\n人格\n\n- 回答简洁直接。\n- 涉及文件时写明路径。"
	if got := b.Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
}

func TestCompleteSemantics(t *testing.T) {
	b := Default()
	if err := b.Complete("只此一段"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// 已累积分节全部让位
	if got := b.Render(); got != "只此一段" {
		t.Fatalf("Render = %q, want %q", got, "只此一段")
	}
	if err := b.Complete("再来一段"); err == nil {
		t.Fatal("重复 Complete 应报错（整段替换只能有一个）")
	}
	if err := b.Add(Section{Name: "x", Text: "x"}); err == nil {
		t.Fatal("Complete 之后再 Add 应报错（两种装配方式二选一）")
	}
	if err := b.Set(Section{Name: "x", Text: "x"}); err == nil {
		t.Fatal("Complete 之后再 Set 应报错")
	}
	// 前半段分节不影响后续 Clone 后 Complete 的独立性
	other := Default().Clone()
	_ = other.Complete("子代理正文")
	if got := other.Render(); got != "子代理正文" {
		t.Fatalf("Clone 后 Render = %q, want %q", got, "子代理正文")
	}
}

func TestCloneIndependence(t *testing.T) {
	base := Default()
	clone := base.Clone()
	if err := clone.Set(Section{Name: "identity", Order: OrderIdentity, Text: "子代理身份"}); err != nil {
		t.Fatalf("clone Set: %v", err)
	}
	if err := clone.Add(Section{Name: "persona", Order: OrderPersona, Text: "子代理人格"}); err != nil {
		t.Fatalf("clone Add: %v", err)
	}
	// 基座不受克隆体影响
	if strings.Contains(base.Render(), "子代理") {
		t.Fatal("基座不应看到克隆体的改动")
	}
	if !strings.Contains(clone.Render(), "子代理身份") || !strings.Contains(clone.Render(), "子代理人格") {
		t.Fatalf("克隆体应包含自己的改动:\n%s", clone.Render())
	}
}

// TestDefaultGolden 是金样例：默认装配的输出一旦变化必然可见（提示词是模型行为
// 的一部分，改一个字都该过目）。
func TestDefaultGolden(t *testing.T) {
	want := strings.Join([]string{
		"你是运行在 MoS 智能体框架里的助手，通过调用工具完成用户交代的任务。",
		"- 回答简洁直接。\n- 涉及文件时写明路径。",
	}, "\n\n")
	if got := Default().Render(); got != want {
		t.Fatalf("Default().Render =\n%s\nwant\n%s", got, want)
	}
}

func TestAddToolGuidelines(t *testing.T) {
	reg := tool.NewRegistry()
	mustRegister := func(ts ...tool.Tool) {
		t.Helper()
		for _, tl := range ts {
			if err := reg.Register(tl); err != nil {
				t.Fatalf("Register(%s): %v", tl.Name(), err)
			}
		}
	}
	mustRegister(
		guidedTool{name: "alpha", text: "甲的指引"},
		plainTool{name: "mute"},            // 不实现 Guideline，不贡献
		guidedTool{name: "beta", text: ""}, // 空指引，不贡献
		guidedTool{name: "gamma", text: "丙的指引"},
	)
	// 可见性过滤：gamma 被隐藏 → 指引同样不进提示词（与 List() 同一语义）
	reg.SetVisibility(func(name string) bool { return name != "gamma" })

	b := Default()
	if err := b.AddToolGuidelines(reg.Guidelines()); err != nil {
		t.Fatalf("AddToolGuidelines: %v", err)
	}
	got := b.Render()
	want := strings.Join([]string{
		"你是运行在 MoS 智能体框架里的助手，通过调用工具完成用户交代的任务。",
		"- 回答简洁直接。\n- 涉及文件时写明路径。",
		"甲的指引",
	}, "\n\n")
	if got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}

	// 指引分节占名 tool:<工具名>：宿主再手工加同名节应报重名
	if err := b.Add(Section{Name: "tool:alpha", Text: "重复"}); err == nil {
		t.Fatal("tool:alpha 已被指引占用，重名 Add 应报错")
	}
}

func TestAddToolGuidelinesRejectsCollision(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(guidedTool{name: "alpha", text: "撞名"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	b := Default()
	// 宿主已手工占用了 tool:alpha → 指引装配当场暴露，不静默覆盖
	if err := b.Add(Section{Name: "tool:alpha", Text: "宿主手写"}); err != nil {
		t.Fatalf("预置分节: %v", err)
	}
	if err := b.AddToolGuidelines(reg.Guidelines()); err == nil {
		t.Fatal("与宿主手写节撞名应报错")
	}
}

func TestAddProjectContextLandsBetweenPolicyAndTools(t *testing.T) {
	b := New()
	if err := b.Add(Section{Name: "identity", Order: OrderIdentity, Text: "身份"}); err != nil {
		t.Fatalf("Add(identity): %v", err)
	}
	if err := b.Add(Section{Name: "rules", Order: OrderPolicy, Text: "约定"}); err != nil {
		t.Fatalf("Add(rules): %v", err)
	}
	if err := b.AddProjectContext("项目约定正文"); err != nil {
		t.Fatalf("AddProjectContext: %v", err)
	}
	if err := b.Add(Section{Name: "tool:read", Order: OrderToolBase, Text: "工具指引"}); err != nil {
		t.Fatalf("Add(tool:read): %v", err)
	}
	want := strings.Join([]string{"身份", "约定", "项目约定正文", "工具指引"}, "\n\n")
	if got := b.Render(); got != want {
		t.Fatalf("Render =\n%s\nwant\n%s", got, want)
	}
}

func TestAddProjectContextEmptyTextAddsNothing(t *testing.T) {
	b := New()
	if err := b.AddProjectContext("   \n\t"); err != nil {
		t.Fatalf("AddProjectContext: %v", err)
	}
	if got := b.Render(); got != "" {
		t.Fatalf("空正文不该留下分节：%q", got)
	}
}

func TestAddProjectContextRejectsDuplicate(t *testing.T) {
	b := New()
	if err := b.AddProjectContext("第一份"); err != nil {
		t.Fatalf("AddProjectContext: %v", err)
	}
	if err := b.AddProjectContext("第二份"); err == nil {
		t.Fatal("重复注入项目上下文应报错（装配错误当场暴露）")
	}
}
