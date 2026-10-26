package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type calcParams struct {
	A int `json:"a" mos:"desc=加数,required=true"`
	B int `json:"b" mos:"desc=加数,required=true"`
}

type calc struct{}

func (calc) Name() string        { return "calc" }
func (calc) Description() string { return "计算两数之和" }
func (calc) Parameters() any     { return &calcParams{} }
func (calc) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := Parse[calcParams](args)
	if err != nil {
		return nil, err
	}
	return p.A + p.B, nil
}

type echo struct{}

func (echo) Name() string                                          { return "echo" }
func (echo) Description() string                                   { return "回显" }
func (echo) Parameters() any                                       { return &struct{}{} }
func (echo) Execute(context.Context, json.RawMessage) (any, error) { return "echo", nil }

type nilTool struct{}

func (nilTool) Name() string        { return "" }
func (nilTool) Description() string { return "" }
func (nilTool) Parameters() any     { return nil }
func (nilTool) Execute(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}

// guided 实现 tool.Guideline 的测试工具。
type guided struct {
	name, text string
}

func (g guided) Name() string        { return g.name }
func (g guided) Description() string { return "带指引的测试工具" }
func (g guided) Parameters() any     { return &struct{}{} }
func (g guided) Guideline() string   { return g.text }
func (g guided) Execute(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}

func TestRegistryRegisterAndCall(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(calc{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(echo{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 注册顺序稳定（利于 prompt cache）
	if got := r.Names(); !reflect.DeepEqual(got, []string{"calc", "echo"}) {
		t.Fatalf("Names = %v, want [calc echo]", got)
	}

	if _, ok := r.Get("calc"); !ok {
		t.Fatal("Get(calc) 应命中")
	}

	out, err := r.Call(context.Background(), "calc", json.RawMessage(`{"a":2,"b":3}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out != 5 {
		t.Fatalf("Call = %v, want 5", out)
	}
}

// fail-closed：重复注册、nil、无名工具一律报错，不静默覆盖。
func TestRegistryRejectsBadRegistration(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(calc{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(calc{}); err == nil {
		t.Fatal("重复注册应报错")
	}
	if err := r.Register(nil); err == nil {
		t.Fatal("nil 工具应报错")
	}
	if err := r.Register(nilTool{}); err == nil {
		t.Fatal("无名工具应报错")
	}
	if len(r.Names()) != 1 {
		t.Fatalf("失败的注册不应进入注册表: %v", r.Names())
	}
}

// fail-closed：调用未注册工具直接报错。
func TestCallUnknownTool(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Call(context.Background(), "ghost", nil); err == nil {
		t.Fatal("未知工具应报错，不得落到任意实现")
	}
}

func TestRegistryList(t *testing.T) {
	r := NewRegistry()
	r.Register(calc{})
	list := r.List()
	if len(list) != 1 {
		t.Fatalf("List 长度 = %d", len(list))
	}
	if list[0].Name != "calc" || list[0].Description != "计算两数之和" {
		t.Fatalf("List[0] = %+v", list[0])
	}
	props, ok := list[0].Parameters.(map[string]any)["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters 应含 properties: %+v", list[0].Parameters)
	}
	if props["a"].(map[string]any)["description"] != "加数" {
		t.Fatalf("mos desc 未进 schema: %+v", props["a"])
	}
}

// Guidelines 只收实现 Guideline 的工具：按注册序、受可见性过滤、
// 空指引不贡献（与 List() 同一可见性语义）。
func TestRegistryGuidelines(t *testing.T) {
	r := NewRegistry()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	must(r.Register(guided{name: "alpha", text: "甲的指引"}))
	must(r.Register(calc{}))                              // 不实现 Guideline
	must(r.Register(guided{name: "beta", text: ""}))      // 空指引不贡献
	must(r.Register(guided{name: "gamma", text: "丙的指引"})) // 会被隐藏
	r.SetVisibility(func(name string) bool { return name != "gamma" })

	got := r.Guidelines()
	if len(got) != 1 || got[0].Tool != "alpha" || got[0].Text != "甲的指引" {
		t.Fatalf("Guidelines = %+v, want 仅 alpha 的指引", got)
	}
}

// Schema 在注册时生成一次并缓存（List 不重复反射）。
func TestSchemaCachedAtRegister(t *testing.T) {
	r := NewRegistry()
	r.Register(calc{})
	first := r.List()[0].Parameters
	second := r.List()[0].Parameters
	if !reflect.DeepEqual(first, second) {
		t.Fatal("List 应返回同一份缓存 schema")
	}
}

func TestGenerateSchema(t *testing.T) {
	schema := generateSchema(&struct {
		Name string   `json:"name" mos:"desc=姓名,required=true"`
		Age  int      `json:"age" mos:"desc=年龄"`
		Memo string   `json:"memo,omitempty" mos:"desc=备注"`
		Tags []string `json:"tags"`
		Rate float64  `json:"rate"`
		OK   bool     `json:"ok"`
		Ptr  *string  `json:"ptr"`
		Skip string   `json:"-"`
	}{})

	if schema["type"] != "object" {
		t.Fatalf("type = %v", schema["type"])
	}
	props := schema["properties"].(map[string]any)
	if _, ok := props["Skip"]; ok {
		t.Fatal("json:\"-\" 字段应跳过")
	}
	if props["tags"].(map[string]any)["type"] != "array" {
		t.Fatalf("slice 应映射为 array: %+v", props["tags"])
	}
	if props["rate"].(map[string]any)["type"] != "number" {
		t.Fatalf("float 应映射为 number: %+v", props["rate"])
	}
	if props["ok"].(map[string]any)["type"] != "boolean" {
		t.Fatalf("bool 应映射为 boolean: %+v", props["ok"])
	}
	if props["ptr"].(map[string]any)["type"] != "string" {
		t.Fatalf("指针应解引用为元素类型: %+v", props["ptr"])
	}
	if props["name"].(map[string]any)["description"] != "姓名" {
		t.Fatalf("desc 未进 schema: %+v", props["name"])
	}

	// 必填采用显式声明：只有 required=true 的字段进 required 数组。
	req, ok := schema["required"].([]string)
	if !ok {
		t.Fatalf("required 缺失: %+v", schema)
	}
	if !reflect.DeepEqual(req, []string{"name"}) {
		t.Fatalf("required = %v, want [name]（显式声明才必填）", req)
	}
}

func TestGenerateSchemaNilAndEmpty(t *testing.T) {
	if got := generateSchema(nil); got["type"] != "object" {
		t.Fatalf("nil 参数应退化为空 object, got %+v", got)
	}
	if got := generateSchema(&struct{}{}); got["type"] != "object" {
		t.Fatalf("空结构体 = %+v", got)
	}
	if got := generateSchema("string"); got["type"] != "string" {
		t.Fatalf("非结构体应按其类型映射, got %+v", got)
	}
}

// 模型偶发输出 "{…}{…}" 拼接的参数：应合并为单一对象而不是解析失败。
func TestNormalizeArgsMergesConcatenatedObjects(t *testing.T) {
	got, err := Parse[map[string]any](json.RawMessage(`{"a":1}{"b":2}`))
	if err != nil {
		t.Fatalf("拼接对象应可解析: %v", err)
	}
	if got["a"] != float64(1) || got["b"] != float64(2) {
		t.Fatalf("拼接对象应合并: %+v", got)
	}
}

func TestParse(t *testing.T) {
	p, err := Parse[calcParams](json.RawMessage(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.A != 1 || p.B != 2 {
		t.Fatalf("Parse = %+v", p)
	}
	// 空参数返回零值而非报错
	var q calcParams
	if q, err = Parse[calcParams](nil); err != nil || q != (calcParams{}) {
		t.Fatalf("空参数应得零值: %+v err=%v", q, err)
	}
	// 非法 JSON 报错（且能带出上下文）
	if _, err := Parse[calcParams](json.RawMessage(`{oops`)); err == nil ||
		!strings.Contains(err.Error(), "解析参数") {
		t.Fatalf("非法 JSON 应报错: %v", err)
	}
}

func TestNamesReturnsCopy(t *testing.T) {
	r := NewRegistry()
	r.Register(calc{})
	names := r.Names()
	names[0] = "hacked"
	if r.Names()[0] != "calc" {
		t.Fatal("Names 必须返回副本")
	}
}
