package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type calcParams struct {
	A int `json:"a" mos:"desc=加数"`
	B int `json:"b" mos:"desc=被加数"`
}

type calculator struct{}

func (calculator) Name() string        { return "calculator" }
func (calculator) Description() string { return "计算两个整数之和" }
func (calculator) Parameters() any     { return &calcParams{} }
func (calculator) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := Parse[calcParams](args)
	if err != nil {
		return nil, err
	}
	return p.A + p.B, nil
}

func TestGenerateSchema(t *testing.T) {
	schema := generateSchema(&struct {
		Name string `json:"name" mos:"desc=姓名"`
		Age  int    `json:"age" mos:"desc=年龄"`
		Memo string `json:"memo,omitempty" mos:"desc=备注"`
	}{})

	if schema["type"] != "object" {
		t.Fatalf("type = %v, want object", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing")
	}
	if name, ok := props["name"].(map[string]any); !ok || name["type"] != "string" || name["description"] != "姓名" {
		t.Fatalf("name schema = %v", props["name"])
	}
	if age, ok := props["age"].(map[string]any); !ok || age["type"] != "integer" {
		t.Fatalf("age schema = %v", props["age"])
	}
	required, ok := schema["required"].([]string)
	if !ok {
		t.Fatalf("required missing")
	}
	if !reflect.DeepEqual(required, []string{"name", "age"}) {
		t.Fatalf("required = %v, want [name age]", required)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(calculator{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 重名注册应报错
	if err := r.Register(calculator{}); err == nil {
		t.Fatal("duplicate register should fail")
	}

	got, ok := r.Get("calculator")
	if !ok {
		t.Fatal("tool not found")
	}
	if got.Name() != "calculator" {
		t.Fatalf("name = %s", got.Name())
	}

	res, err := r.Call(context.Background(), "calculator", json.RawMessage(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res != 3 {
		t.Fatalf("result = %v, want 3", res)
	}

	list := r.List()
	if len(list) != 1 || list[0].Name != "calculator" {
		t.Fatalf("list = %+v", list)
	}
	if _, ok := list[0].Parameters.(map[string]any); !ok {
		t.Fatalf("parameters not a schema map: %T", list[0].Parameters)
	}

	if _, err := r.Call(context.Background(), "missing", nil); err == nil {
		t.Fatal("calling unknown tool should fail")
	}
}

type calcA struct{ calculator }

func (calcA) Name() string { return "calcA" }

type calcB struct{ calculator }

func (calcB) Name() string { return "calcB" }

// TestRegistryListOrder 验证 List/Names 按注册顺序稳定返回（而非 map 随机遍历），
// 且 Schema 来自注册时缓存（多次 List 结果一致）。
func TestRegistryListOrder(t *testing.T) {
	r := NewRegistry()
	for _, tt := range []Tool{calcA{}, calcB{}} {
		if err := r.Register(tt); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	want := []string{"calcA", "calcB"}
	if got := r.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	first := r.List()
	second := r.List()
	if len(first) != 2 {
		t.Fatalf("List len = %d, want 2", len(first))
	}
	for i, name := range want {
		if first[i].Name != name {
			t.Errorf("List[%d].Name = %s, want %s", i, first[i].Name, name)
		}
		if !reflect.DeepEqual(first[i].Parameters, second[i].Parameters) {
			t.Errorf("schema unstable between List calls for %s", name)
		}
	}
}
