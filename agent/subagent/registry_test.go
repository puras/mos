package subagent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/puras/mos/tool"
)

// stubTool 是只登记名字的工具替身。
type stubTool struct{ name string }

func (s stubTool) Name() string        { return s.name }
func (s stubTool) Description() string { return "工具 " + s.name }
func (s stubTool) Parameters() any     { return &TaskParams{} }
func (s stubTool) Execute(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}

// 父注册表：三个工具，其中一个被可见性层挡掉。
func parentRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg := tool.NewRegistry()
	for _, n := range []string{"read", "grep", "shell"} {
		if err := reg.Register(stubTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	// shell 被宿主策略挡掉（可见性层）。
	reg.SetVisibility(func(name string) bool { return name != "shell" })
	return reg
}

// 白名单交集：声明放大无效（父看不见的工具进不了子注册表）。
func TestChildRegistryIntersects(t *testing.T) {
	parent := parentRegistry(t)
	child, dropped := ChildRegistry(parent, []string{"read", "shell", "grep"})

	if got := child.Names(); len(got) != 2 || got[0] != "grep" || got[1] != "read" {
		t.Fatalf("子注册表应只含交集：%v", got)
	}
	if len(dropped) != 1 || dropped[0] != "shell" {
		t.Fatalf("被挡掉的声明项应被报出来：%v", dropped)
	}
	if _, ok := child.Get("shell"); ok {
		t.Fatal("父看不见的工具不该进子注册表")
	}
}

// 空白名单 → 不收窄：继承父的可见集。
func TestChildRegistryEmptyWhitelistInheritsParent(t *testing.T) {
	parent := parentRegistry(t)
	child, dropped := ChildRegistry(parent, nil)
	if got := child.Names(); len(got) != 2 {
		t.Fatalf("应继承父可见集：%v", got)
	}
	if len(dropped) != 0 {
		t.Fatalf("没声明不该有挡掉项：%v", dropped)
	}
}

// 子注册表里的工具是父注册表里的**同一个实例**（行为一致，不复制实现）。
func TestChildRegistryReusesParentTools(t *testing.T) {
	parent := parentRegistry(t)
	child, _ := ChildRegistry(parent, []string{"read"})
	got, ok := child.Get("read")
	if !ok {
		t.Fatal("子注册表里该有 read")
	}
	want, _ := parent.Get("read")
	if got != want {
		t.Fatal("该复用父注册表里的同一个工具实例")
	}
}

// 父注册表里没有的工具（声明了但没装）→ 不进子注册表，并报出来。
func TestChildRegistryDropsUnknownTools(t *testing.T) {
	parent := parentRegistry(t)
	child, dropped := ChildRegistry(parent, []string{"read", "nonexistent"})
	if got := child.Names(); len(got) != 1 || got[0] != "read" {
		t.Fatalf("没装的工具不该进子注册表：%v", got)
	}
	if len(dropped) != 1 || dropped[0] != "nonexistent" {
		t.Fatalf("没装的声明项应被报出来：%v", dropped)
	}
}

// 没有父注册表 → 空子注册表（全被挡掉）。
func TestChildRegistryWithoutParent(t *testing.T) {
	child, dropped := ChildRegistry(nil, []string{"read"})
	if len(child.Names()) != 0 {
		t.Fatalf("没有父注册表时子注册表应为空：%v", child.Names())
	}
	if len(dropped) != 1 {
		t.Fatalf("声明项应全被报出来：%v", dropped)
	}
}

// 恒定排除：深度 0——子代理不能再委派（`subagent__*` 进不了子注册表）。
func TestChildRegistryExcludesSubagentTools(t *testing.T) {
	parent := tool.NewRegistry()
	for _, n := range []string{"read", "subagent__reviewer"} {
		if err := parent.Register(stubTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	child, dropped := ChildRegistry(parent, []string{"read", "subagent__reviewer"})

	if _, ok := child.Get("subagent__reviewer"); ok {
		t.Fatal("子代理不该继承 subagent__*（深度 0）")
	}
	if got := child.Names(); len(got) != 1 || got[0] != "read" {
		t.Fatalf("子注册表应只剩 read：%v", got)
	}
	if len(dropped) != 1 || dropped[0] != "subagent__reviewer" {
		t.Fatalf("被挡掉的 subagent__* 应报出来：%v", dropped)
	}
}

// 恒定排除：工具面不经 tool_search（`tool_search` 进不了子注册表）。
func TestChildRegistryExcludesToolSearch(t *testing.T) {
	parent := tool.NewRegistry()
	for _, n := range []string{"read", "tool_search"} {
		if err := parent.Register(stubTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	child, dropped := ChildRegistry(parent, []string{"read", "tool_search"})

	if _, ok := child.Get("tool_search"); ok {
		t.Fatal("子代理不该继承 tool_search（工具面不经 search）")
	}
	if got := child.Names(); len(got) != 1 || got[0] != "read" {
		t.Fatalf("子注册表应只剩 read：%v", got)
	}
	if len(dropped) != 1 || dropped[0] != "tool_search" {
		t.Fatalf("被挡掉的 tool_search 应报出来：%v", dropped)
	}
}

// 空白名单时也恒定排除这两类（不然「没声明」就等于「全继承」）。
func TestChildRegistryExcludesEvenWithEmptyWhitelist(t *testing.T) {
	parent := tool.NewRegistry()
	for _, n := range []string{"read", "tool_search", "subagent__reviewer"} {
		if err := parent.Register(stubTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	child, _ := ChildRegistry(parent, nil)

	for _, n := range []string{"tool_search", "subagent__reviewer"} {
		if _, ok := child.Get(n); ok {
			t.Fatalf("%s 不该进子注册表：%v", n, child.Names())
		}
	}
	if got := child.Names(); len(got) != 1 || got[0] != "read" {
		t.Fatalf("子注册表应只剩 read：%v", got)
	}
}
