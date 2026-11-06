package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// sTool 是可配 schema 的测试工具。
type sTool struct {
	name   string
	desc   string
	params any
	schema any // 非 nil = 实现 SchemaProvider
	out    any
}

func (t *sTool) Name() string        { return t.name }
func (t *sTool) Description() string { return t.desc }
func (t *sTool) Parameters() any     { return t.params }
func (t *sTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return t.out, nil
}
func (t *sTool) Schema() any { return t.schema }

func namesOf(r *Registry) []string {
	var out []string
	for _, tl := range r.List() {
		out = append(out, tl.Name)
	}
	return out
}

// SchemaProvider：自带的 schema 原样进 List()，Parameters 的反射结果被忽略。
func TestSchemaProviderDirect(t *testing.T) {
	upstream := map[string]any{
		"type": "object",
		"properties": map[string]any{
			// 上游形态：title / 多类型数组（MoS 反射生成不出这些——正是直通的意义）
			"path": map[string]any{"title": "路径", "type": []any{"string", "null"}},
		},
		"required": []any{"path"},
	}
	r := NewRegistry()
	if err := r.Register(&sTool{
		name: "mcp__srv__find",
		desc: "上游工具",
		// 反射来源故意给个不一样的：若被用到，List 的 schema 不会等于 upstream
		params: &struct{}{},
		schema: upstream,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	list := r.List()
	if len(list) != 1 {
		t.Fatalf("清单 = %d 项", len(list))
	}
	b, _ := json.Marshal(list[0].Parameters)
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("schema 不是 JSON 对象: %v", err)
	}
	props, _ := got["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("schema 缺 properties: %s", b)
	}
	path, _ := props["path"].(map[string]any)
	if path == nil || path["title"] == nil || path["type"] == nil {
		t.Fatalf("schema 应原样透传上游字段（含嵌套层）: %s", b)
	}
	req, _ := got["required"].([]any)
	if len(req) != 1 || req[0] != "path" {
		t.Fatalf("required 应原样透传: %s", b)
	}
}

// SchemaProvider 返回 nil → 降级反射（fail-open 到老路）。
func TestSchemaProviderNilFallsBackToReflection(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&sTool{
		name: "x",
		desc: "降级",
		params: &struct {
			Query string `json:"query" mos:"desc=查询,required=true"`
		}{},
		schema: nil,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	list := r.List()
	b, _ := json.Marshal(list[0].Parameters)
	if !contains(string(b), `"required"`) || !contains(string(b), `"query"`) {
		t.Fatalf("应走反射生成: %s", b)
	}
}

// 自带 schema 非对象形态：归一失败降级宽松 object，不 panic。
func TestSchemaProviderGarbageSchemaDegrades(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&sTool{name: "bad", desc: "坏 schema", schema: "不是 schema"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	list := r.List()
	b, _ := json.Marshal(list[0].Parameters)
	if !contains(string(b), `"type":"object"`) {
		t.Fatalf("坏 schema 应降级宽松 object: %s", b)
	}
}

// AddVisibility：两层叠加，AND 语义；谁也不覆盖谁。
func TestAddVisibilityStacks(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"a", "b", "c"} {
		if err := r.Register(&sTool{name: n, desc: "x", params: &struct{}{}}); err != nil {
			t.Fatalf("Register %s: %v", n, err)
		}
	}
	// 第一层：policy 只放 a、b。
	r.SetVisibility(func(name string) bool { return name == "a" || name == "b" })
	if got := namesOf(r); len(got) != 2 {
		t.Fatalf("单层过滤后 = %v", got)
	}
	// 第二层：MCP hidden 再藏 b。AND 之后只剩 a——两层各管各的。
	r.AddVisibility(func(name string) bool { return name != "b" })
	if got := namesOf(r); len(got) != 1 || got[0] != "a" {
		t.Fatalf("叠加过滤后应只剩 a：%v", got)
	}
	// Call 不受可见性影响（隐藏 ≠ 禁用）。
	if _, err := r.Call(context.Background(), "b", []byte(`{}`)); err != nil {
		t.Fatalf("被隐藏的工具仍可 Call: %v", err)
	}
	// SetVisibility 再设 = 替换整个栈。
	r.SetVisibility(func(string) bool { return true })
	if got := namesOf(r); len(got) != 3 {
		t.Fatalf("SetVisibility 应清掉旧栈：%v", got)
	}
}

// Replace：原子换代——新名追加、同名覆盖、上一代消失、schema 与 deferred
// 同步；手动注册的内置工具不受影响；校验失败注册表保持原样。
func TestReplace(t *testing.T) {
	r := NewRegistry()
	builtin := &sTool{name: "read", desc: "内置", params: &struct{}{}}
	if err := r.Register(builtin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	v1 := []*sTool{
		{name: "mcp__s__a", desc: "v1 a", params: &struct{}{}},
		{name: "mcp__s__b", desc: "v1 b", params: &struct{}{}},
	}
	if err := r.Replace("mcp:s", []Tool{v1[0], v1[1]}, map[string]bool{"mcp__s__a": true, "mcp__s__b": true}); err != nil {
		t.Fatalf("Replace v1: %v", err)
	}
	// deferred 态：不进 List；schema 已缓存。
	if got := namesOf(r); len(got) != 1 {
		t.Fatalf("deferred 工具不该进清单：%v", got)
	}
	if _, err := r.Activate([]string{"mcp__s__a"}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := namesOf(r); len(got) != 2 {
		t.Fatalf("激活后应有 a：%v", got)
	}
	// 换代 v2：a 换描述、b 消失、c 新增（deferred）。
	v2 := []*sTool{
		{name: "mcp__s__a", desc: "v2 a", params: &struct{}{}},
		{name: "mcp__s__c", desc: "v2 c", params: &struct{}{}},
	}
	if err := r.Replace("mcp:s", []Tool{v2[0], v2[1]}, map[string]bool{"mcp__s__c": true}); err != nil {
		t.Fatalf("Replace v2: %v", err)
	}
	got := namesOf(r)
	if len(got) != 2 || got[0] != "read" || got[1] != "mcp__s__a" {
		t.Fatalf("换代后清单 = %v（read 与 a 应在，b 应消失）", got)
	}
	for _, tl := range r.List() {
		if tl.Name == "mcp__s__a" && tl.Description != "v2 a" {
			t.Fatalf("a 应换成 v2：%q", tl.Description)
		}
	}
	// c 是新 deferred；b 的 deferred 标记应随换代消失（它已不在注册表）。
	if _, ok := r.Get("mcp__s__b"); ok {
		t.Fatal("b 应已从注册表消失")
	}
	if len(r.DeferredTools()) != 1 || r.DeferredTools()[0].Name != "mcp__s__c" {
		t.Fatalf("deferred 应只剩 c：%v", r.DeferredTools())
	}
	// 内置工具不受换代波及。
	if _, ok := r.Get("read"); !ok {
		t.Fatal("名单外的内置工具不该被动")
	}
}

// Replace 校验失败：整批拒绝，注册表保持调用前的样子（不留半套）。
func TestReplaceRejectedAtomically(t *testing.T) {
	r := NewRegistry()
	v1 := &sTool{name: "mcp__s__a", desc: "v1", params: &struct{}{}}
	if err := r.Replace("mcp:s", []Tool{v1}, nil); err != nil {
		t.Fatalf("Replace v1: %v", err)
	}
	// 名单内重名 → 拒绝。
	err := r.Replace("mcp:s", []Tool{
		&sTool{name: "mcp__s__a", desc: "x", params: &struct{}{}},
		&sTool{name: "mcp__s__a", desc: "y", params: &struct{}{}},
	}, nil)
	if err == nil {
		t.Fatal("名单内重名应报错")
	}
	// deferred 名单含名单外工具 → 拒绝。
	err = r.Replace("mcp:s", []Tool{&sTool{name: "mcp__s__b", desc: "x", params: &struct{}{}}},
		map[string]bool{"mcp__s__b": true, "mcp__s__ghost": true})
	if err == nil {
		t.Fatal("deferred 名单含名单外工具应报错")
	}
	// 两次失败之后，注册表仍是 v1 的样子。
	got := namesOf(r)
	if len(got) != 1 || got[0] != "mcp__s__a" {
		t.Fatalf("失败的 Replace 不该改动注册表：%v", got)
	}
	list := r.List()
	if list[0].Description != "v1" {
		t.Fatalf("描述应还是 v1：%q", list[0].Description)
	}
}

// 代际隔离：不同 scope 的换代互不踩（服务器 A 换代不动服务器 B 的工具）。
func TestReplaceScopeIsolation(t *testing.T) {
	r := NewRegistry()
	a1 := &sTool{name: "mcp__a__x", desc: "a1", params: &struct{}{}}
	b1 := &sTool{name: "mcp__b__y", desc: "b1", params: &struct{}{}}
	if err := r.Replace("mcp:a", []Tool{a1}, nil); err != nil {
		t.Fatalf("Replace a: %v", err)
	}
	if err := r.Replace("mcp:b", []Tool{b1}, nil); err != nil {
		t.Fatalf("Replace b: %v", err)
	}
	// a 换代：a 的旧工具消失，b 的不受影响。
	a2 := &sTool{name: "mcp__a__z", desc: "a2", params: &struct{}{}}
	if err := r.Replace("mcp:a", []Tool{a2}, nil); err != nil {
		t.Fatalf("Replace a v2: %v", err)
	}
	if _, ok := r.Get("mcp__a__x"); ok {
		t.Fatal("a 的上一代工具应消失")
	}
	if _, ok := r.Get("mcp__b__y"); !ok {
		t.Fatal("b 的工具不该被 a 的换代波及")
	}
	if _, ok := r.Get("mcp__a__z"); !ok {
		t.Fatal("a 的新工具应在")
	}
}

// 空名单：该 scope 的工具全撤（服务器撤掉全部工具 ≠ 装配错误），
// 其他 scope 不受影响。
func TestReplaceEmptyScopeClearsOnlyThatScope(t *testing.T) {
	r := NewRegistry()
	a1 := &sTool{name: "mcp__a__x", desc: "a1", params: &struct{}{}}
	b1 := &sTool{name: "mcp__b__y", desc: "b1", params: &struct{}{}}
	if err := r.Replace("mcp:a", []Tool{a1}, nil); err != nil {
		t.Fatalf("Replace a: %v", err)
	}
	if err := r.Replace("mcp:b", []Tool{b1}, nil); err != nil {
		t.Fatalf("Replace b: %v", err)
	}
	// a 换成空名单：a 的工具全没，b 的还在。
	if err := r.Replace("mcp:a", nil, nil); err != nil {
		t.Fatalf("Replace a（空名单）: %v", err)
	}
	if _, ok := r.Get("mcp__a__x"); ok {
		t.Fatal("空名单应清掉该 scope 的工具")
	}
	if _, ok := r.Get("mcp__b__y"); !ok {
		t.Fatal("空名单不该影响其他 scope")
	}
	if got := namesOf(r); len(got) != 1 || got[0] != "mcp__b__y" {
		t.Fatalf("清单 = %v", got)
	}
}

// deferred 标记重写：上一代 deferred、这一代 direct → 工具要真的可见
// （不能遗留旧标记导致“永远隐身”）。
func TestReplaceRewritesDeferredFlags(t *testing.T) {
	r := NewRegistry()
	v1 := &sTool{name: "mcp__s__a", desc: "v1", params: &struct{}{}}
	if err := r.Replace("mcp:s", []Tool{v1}, map[string]bool{"mcp__s__a": true}); err != nil {
		t.Fatalf("Replace v1: %v", err)
	}
	if got := namesOf(r); len(got) != 0 {
		t.Fatalf("deferred 时不该进清单：%v", got)
	}
	// 同一工具这一代改成 direct。
	v2 := &sTool{name: "mcp__s__a", desc: "v2", params: &struct{}{}}
	if err := r.Replace("mcp:s", []Tool{v2}, map[string]bool{"mcp__s__a": false}); err != nil {
		t.Fatalf("Replace v2: %v", err)
	}
	got := namesOf(r)
	if len(got) != 1 || got[0] != "mcp__s__a" {
		t.Fatalf("deferred → direct 后应进清单：%v", got)
	}
	if len(r.DeferredTools()) != 0 {
		t.Fatalf("不该再有 deferred 工具：%v", r.DeferredTools())
	}
	// 反向：direct → deferred → 不进清单。
	v3 := &sTool{name: "mcp__s__a", desc: "v3", params: &struct{}{}}
	if err := r.Replace("mcp:s", []Tool{v3}, map[string]bool{"mcp__s__a": true}); err != nil {
		t.Fatalf("Replace v3: %v", err)
	}
	if got := namesOf(r); len(got) != 0 {
		t.Fatalf("direct → deferred 后不该进清单：%v", got)
	}
	if len(r.DeferredTools()) != 1 || r.DeferredTools()[0].Name != "mcp__s__a" {
		t.Fatalf("deferred 集应只剩它：%v", r.DeferredTools())
	}
}

// 具名槽：替换某一槽不影响其他槽（policy 热替换不踩 MCP 层）。
func TestVisibilitySlotIsolation(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"a", "b"} {
		if err := r.Register(&sTool{name: n, desc: n, params: &struct{}{}}); err != nil {
			t.Fatalf("Register %s: %v", n, err)
		}
	}
	r.SetVisibilitySlot("policy", func(name string) bool { return name != "a" })
	r.SetVisibilitySlot("mcp", func(name string) bool { return name != "b" })
	if got := namesOf(r); len(got) != 0 {
		t.Fatalf("两层各挡一个，应都看不见：%v", got)
	}
	// 换掉 policy 槽：mcp 槽仍在（b 仍被挡，a 恢复可见）。
	r.SetVisibilitySlot("policy", func(name string) bool { return true })
	if got := namesOf(r); len(got) != 1 || got[0] != "a" {
		t.Fatalf("换 policy 槽后应只剩 a 可见：%v", got)
	}
	// 移除 mcp 槽：两个都可见。
	r.SetVisibilitySlot("mcp", nil)
	if got := namesOf(r); len(got) != 2 {
		t.Fatalf("移除 mcp 槽后应都可见：%v", got)
	}
}

// 守护：结构体工具的反射 schema 与 A 片之前逐字节一致（SchemaProvider 不能
// 改变老工具的行为——FR-C2 守护的延伸）。
func TestStructToolSchemaUnchanged(t *testing.T) {
	type Args struct {
		Query string `json:"query" mos:"desc=查询,required=true"`
		Limit int    `json:"limit,omitempty" mos:"desc=上限"`
	}
	r := NewRegistry()
	if err := r.Register(&sTool{name: "s", desc: "d", params: &Args{}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	b, _ := json.Marshal(r.List()[0].Parameters)
	want := `{"properties":{"limit":{"description":"上限","type":"integer"},"query":{"description":"查询","type":"string"}},"required":["query"],"type":"object"}`
	if string(b) != want {
		t.Fatalf("反射 schema 变了：\n got %s\nwant %s", b, want)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
