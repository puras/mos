package tool

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeTool struct{ name string }

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return "工具 " + f.name }
func (f fakeTool) Parameters() any     { return struct{}{} }
func (f fakeTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return nil, nil
}

// RegisterDeferred 注册后不进 List，名字与描述进 DeferredTools。
func TestRegisterDeferredHiddenFromList(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(fakeTool{name: "normal"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.RegisterDeferred(fakeTool{name: "lazy"}); err != nil {
		t.Fatalf("RegisterDeferred: %v", err)
	}

	defs := reg.List()
	if len(defs) != 1 || defs[0].Name != "normal" {
		t.Fatalf("deferred 工具不该出现在 List(): %+v", defs)
	}
	if reg.Guidelines() != nil {
		t.Fatalf("Guidelines 不该受 deferred 影响（本例无指引）: %+v", reg.Guidelines())
	}

	d := reg.DeferredTools()
	if len(d) != 1 || d[0].Name != "lazy" || d[0].Description != "工具 lazy" {
		t.Fatalf("DeferredTools = %+v, want [lazy]", d)
	}

	// Call 不受 deferred 影响：执行与否由守卫决定，不在可见层拦。
	if _, ok := reg.Get("lazy"); !ok {
		t.Fatal("Get 应能拿到 deferred 工具")
	}
}

// Activate 激活后下一轮 List 生效；重复激活幂等；未注册报错。
func TestActivateRoundTrip(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(fakeTool{name: "normal"})
	_ = reg.RegisterDeferred(fakeTool{name: "a"})
	_ = reg.RegisterDeferred(fakeTool{name: "b"})

	if _, err := reg.Activate([]string{"nope"}); err == nil {
		t.Fatal("激活未注册工具应报错")
	}

	activated, err := reg.Activate([]string{"a", "normal", "a"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(activated) != 1 || activated[0] != "a" {
		t.Fatalf("activated = %v, want [a]（normal 非 deferred 跳过；a 重复不重复算）", activated)
	}
	defs := reg.List()
	if len(defs) != 2 || defs[0].Name != "normal" || defs[1].Name != "a" {
		t.Fatalf("激活后 List = %+v, want [normal a]（保持注册序）", defs)
	}
	if d := reg.DeferredTools(); len(d) != 1 || d[0].Name != "b" {
		t.Fatalf("已激活的不该再列: %+v", d)
	}
}

// 可见性过滤与 deferred 是两道独立的门：deferred 工具激活后被可见性过滤掉
// 依旧不可见。
func TestDeferredAndVisibilityIndependent(t *testing.T) {
	reg := NewRegistry()
	_ = reg.RegisterDeferred(fakeTool{name: "hidden_lazy"})
	reg.SetVisibility(func(name string) bool { return name != "hidden_lazy" })

	activated, err := reg.Activate([]string{"hidden_lazy"})
	if err != nil || len(activated) != 1 {
		t.Fatalf("Activate = %v, %v", activated, err)
	}
	if defs := reg.List(); len(defs) != 0 {
		t.Fatalf("可见性过滤仍应生效: %+v", defs)
	}
}
