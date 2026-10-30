package mos_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mos "github.com/puras/mos"
	"github.com/puras/mos/agent/prompt"
	"github.com/puras/mos/tool"
)

type searchStub struct{ name, desc string }

func (s searchStub) Name() string        { return s.name }
func (s searchStub) Description() string { return s.desc }
func (s searchStub) Parameters() any     { return struct{}{} }
func (s searchStub) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return "ok", nil
}

// deferred 集非空 → tool_search 自动注册；没有 deferred 就没有 tool_search，
// Describe 快照如实写明两种状态（能力即证据）。
func TestToolSearchWiring(t *testing.T) {
	withDeferred := tool.NewRegistry()
	if err := withDeferred.Register(searchStub{name: "read", desc: "读文件"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := withDeferred.RegisterDeferred(searchStub{name: "web_search", desc: "联网搜索"}); err != nil {
		t.Fatalf("RegisterDeferred: %v", err)
	}

	a, err := mos.New(mos.Config{Model: &stubModel{}, Tools: withDeferred})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Describe 是对外可观测面：注册与否、快照是否写明，都在这里断言。
	snapshot := a.Describe()
	if !strings.Contains(snapshot, "tool_search: 已装配") {
		t.Fatalf("deferred 非空，Describe 应写明 tool_search 已装配:\n%s", snapshot)
	}
	if !strings.Contains(snapshot, "web_search") {
		t.Fatalf("Describe 应列出 deferred 工具所在（上下文文件行外，工具行应可见）:\n%s", snapshot)
	}

	without := tool.NewRegistry()
	if err := without.Register(searchStub{name: "read", desc: "读文件"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	a2, err := mos.New(mos.Config{Model: &stubModel{}, Tools: without})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(a2.Describe(), "tool_search: 未装配") {
		t.Fatalf("没有 deferred 工具，Describe 应写明 tool_search 未装配:\n%s", a2.Describe())
	}
	// 真正的清单检查：deferred 工具与 tool_search 都不在 Registry 可见清单外泄漏。
	// （a.tools 是私有字段；经由 Run 的真实行为由 react 包的驱动测试覆盖，
	// 这里用 Registry 直查：装配后的注册表就是宿主传入的同一份。）
	for _, def := range withDeferred.List() {
		if def.Name == "web_search" {
			t.Fatal("deferred 工具不该出现在 List()")
		}
	}
}

// 端到端：tool_search 搜索 → 激活 → 清单含被激活工具、未命中保持 deferred。
func TestToolSearchEndToEndActivation(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(searchStub{name: "read", desc: "读文件"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.RegisterDeferred(searchStub{name: "web_search", desc: "联网搜索网页并返回结果"}); err != nil {
		t.Fatalf("RegisterDeferred: %v", err)
	}
	if err := reg.RegisterDeferred(searchStub{name: "sql_query", desc: "查询数据库执行 SQL"}); err != nil {
		t.Fatalf("RegisterDeferred: %v", err)
	}
	if _, err := mos.New(mos.Config{Model: &stubModel{}, Tools: reg}); err != nil {
		t.Fatalf("New: %v", err)
	}

	ts, ok := reg.Get("tool_search")
	if !ok {
		t.Fatal("tool_search 未注册")
	}
	args, _ := json.Marshal(map[string]any{"query": "联网搜索网页"})
	out, err := ts.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	loaded := out.(map[string]any)["loaded"].([]string)
	if len(loaded) != 1 || loaded[0] != "web_search" {
		t.Fatalf("loaded = %v, want [web_search]", loaded)
	}
	defs := reg.List()
	found := false
	for _, d := range defs {
		if d.Name == "web_search" {
			found = true
		}
	}
	if !found {
		t.Fatalf("激活后 List 缺 web_search: %+v", defs)
	}
	if ds := reg.DeferredTools(); len(ds) != 1 || ds[0].Name != "sql_query" {
		t.Fatalf("未命中的保持 deferred: %+v", ds)
	}
}

// FR-C4 守护：Prompt 装配期渲染一次后静态不变——连续两次 Render 逐字节一致
// （SystemPrompt 渲染后进 SystemPrompt 字段，每轮原样发送；稳定性是 prompt
// cache 的前提）。
func TestSystemPromptStableAcrossRounds(t *testing.T) {
	b := prompt.Default()
	first := b.Render()
	second := b.Render()
	if first != second {
		t.Fatal("同一 Builder 两次 Render 应逐字节一致（prompt cache 前提）")
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("Default() 不该渲染出空提示词")
	}
}
