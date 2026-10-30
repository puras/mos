package searchtools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/puras/mos/tool"
)

type stub struct {
	name, desc string
}

func (s stub) Name() string        { return s.name }
func (s stub) Description() string { return s.desc }
func (s stub) Parameters() any     { return struct{}{} }
func (s stub) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return "ok", nil
}

func setup(t *testing.T) (*tool.Registry, tool.Tool) {
	t.Helper()
	reg := tool.NewRegistry()
	if err := reg.Register(stub{name: "read", desc: "读取文件内容"}); err != nil {
		t.Fatalf("Register read: %v", err)
	}
	for _, c := range []stub{
		{name: "web_search", desc: "搜索互联网网页，返回搜索结果"},
		{name: "web_fetch", desc: "抓取网页正文"},
		{name: "db_query", desc: "查询数据库执行 SQL"},
	} {
		if err := reg.RegisterDeferred(c); err != nil {
			t.Fatalf("RegisterDeferred %s: %v", c.name, err)
		}
	}
	ts := NewToolSearch(Options{Registry: reg})
	if err := reg.Register(ts); err != nil {
		t.Fatalf("Register tool_search: %v", err)
	}
	return reg, ts
}

func call(t *testing.T, ts tool.Tool, query string) (string, []string) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"query": query})
	out, err := ts.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute(%q): %v", query, err)
	}
	m := out.(map[string]any)
	return m["text"].(string), m["loaded"].([]string)
}

// 搜索命中即激活：命中项下一轮进 List()；未命中项保持 deferred。
func TestSearchActivatesMatches(t *testing.T) {
	reg, ts := setup(t)

	text, loaded := call(t, ts, "联网搜索资料")
	if len(loaded) != 1 || loaded[0] != "web_search" {
		t.Fatalf("loaded = %v, want [web_search]", loaded)
	}
	if len(reg.DeferredTools()) != 2 {
		t.Fatalf("剩 deferred = %d, want 2", len(reg.DeferredTools()))
	}
	defs := reg.List()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["web_search"] || !names["read"] || !names["tool_search"] {
		t.Fatalf("激活后 List 缺项: %v", names)
	}
	if names["web_fetch"] || names["db_query"] {
		t.Fatal("未命中/未激活的工具不该出现在 List()")
	}
	if !contains(text, "web_search") {
		t.Fatalf("回执应列出激活的工具名: %s", text)
	}
}

// BM25 相关性：查询词与描述匹配的排前面；纯噪声返回空。
func TestRanking(t *testing.T) {
	_, ts := setup(t)

	_, loaded := call(t, ts, "数据库 SQL 查询")
	if len(loaded) == 0 || loaded[0] != "db_query" {
		t.Fatalf("top match = %v, want db_query", loaded)
	}

	text, loaded := call(t, ts, "量子色动力学的烹饪方法")
	if len(loaded) != 0 || !contains(text, "没有匹配") {
		t.Fatalf("无关联查询应空结果: %v %s", loaded, text)
	}
}

// 空 query 报错（fail-closed）；limit 传递。
func TestValidation(t *testing.T) {
	_, ts := setup(t)
	args, _ := json.Marshal(map[string]any{"query": "  "})
	if _, err := ts.Execute(context.Background(), args); err == nil {
		t.Fatal("空 query 应报错")
	}
}

// 再激活一个已激活的工具：Activate 跳过（幂等），不报错。
func TestActivateIdempotent(t *testing.T) {
	reg, _ := setup(t)
	if _, err := reg.Activate([]string{"web_fetch"}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	activated, err := reg.Activate([]string{"web_fetch"})
	if err != nil {
		t.Fatalf("重复 Activate: %v", err)
	}
	if len(activated) != 0 {
		t.Fatalf("重复激活应返回空: %v", activated)
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
