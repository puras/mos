package react

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// recorderPlanner 记录每轮收到的工具清单；剧本只作答（不真正调工具）。
type recorderPlanner struct {
	seenTools [][]llm.Tool
	i         int
}

func (p *recorderPlanner) NextAction(_ context.Context, _ []llm.Message, tools []llm.Tool) (planner.Action, error) {
	p.seenTools = append(p.seenTools, tools)
	a := planner.Action{Content: "done", FinishReason: "stop"}
	p.i++
	return a, nil
}

type echoTool struct{ name string }

func (e echoTool) Name() string        { return e.name }
func (e echoTool) Description() string { return "工具 " + e.name }
func (e echoTool) Parameters() any     { return struct{}{} }
func (e echoTool) Execute(_ context.Context, _ json.RawMessage) (any, error) {
	return "ok", nil
}

// 历史工具名并入：恢复会话后（日志里有 deferred 工具的调用、Registry 重启后
// 又是 deferred 未激活态），驱动组装清单时自动激活，声明清单恢复完整。
func TestHistoryToolNamesReactivated(t *testing.T) {
	// 1) 造一个带 deferred 工具调用记录的会话日志
	reg1 := tool.NewRegistry()
	if err := reg1.RegisterDeferred(echoTool{name: "lazy"}); err != nil {
		t.Fatalf("RegisterDeferred: %v", err)
	}
	sess := session.New("t", nil)
	if _, err := sess.Append(session.Event{Kind: session.EventTurnStart, Turn: 1}); err != nil {
		t.Fatalf("append turnstart: %v", err)
	}
	if _, err := sess.Append(session.Event{Kind: session.EventUser, Turn: 1, Text: "hi"}); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if _, err := sess.Append(session.Event{
		Kind: session.EventAssistant, Turn: 1,
		Text:      "调工具",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "lazy", Args: "{}"}},
	}); err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	if _, err := sess.Append(session.Event{
		Kind: session.EventToolResult, Turn: 1,
		ToolResult: &session.ToolResult{CallID: "c1", Content: "ok"},
	}); err != nil {
		t.Fatalf("append result: %v", err)
	}

	// 2) 新 Registry（模拟重启）：lazy 还是 deferred 未激活态
	reg2 := tool.NewRegistry()
	if err := reg2.RegisterDeferred(echoTool{name: "lazy"}); err != nil {
		t.Fatalf("RegisterDeferred: %v", err)
	}

	p := &recorderPlanner{}
	rt := &driver.RunContext{Planner: p, Tools: reg2}
	d := driver.New(rt, New())
	if _, err := d.Run(context.Background(), sess, "继续"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.seenTools) == 0 {
		t.Fatal("决策器未被调用")
	}
	found := false
	for _, def := range p.seenTools[0] {
		if def.Name == "lazy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("历史里调过的 deferred 工具应被并入声明清单: %+v", p.seenTools[0])
	}
	// 兜底激活后 Registry 状态也恢复：DeferredTools 不再列出。
	if ds := reg2.DeferredTools(); len(ds) != 0 {
		t.Fatalf("激活态应已恢复: %+v", ds)
	}
}

// 幻觉工具名（历史里有、Registry 里没有）不阻断：防御性跳过。
func TestHistoryUnknownToolNameIgnored(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(echoTool{name: "normal"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess := session.New("t", nil)
	_, _ = sess.Append(session.Event{Kind: session.EventTurnStart, Turn: 1})
	_, _ = sess.Append(session.Event{Kind: session.EventUser, Turn: 1, Text: "hi"})
	_, _ = sess.Append(session.Event{
		Kind: session.EventAssistant, Turn: 1,
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "ghost", Args: "{}"}},
	})
	_, _ = sess.Append(session.Event{
		Kind: session.EventToolResult, Turn: 1,
		ToolResult: &session.ToolResult{CallID: "c1", Content: "ok", IsError: true},
	})

	p := &recorderPlanner{}
	rt := &driver.RunContext{Planner: p, Tools: reg}
	d := driver.New(rt, New())
	if _, err := d.Run(context.Background(), sess, "hi"); err != nil {
		t.Fatalf("未注册的历史工具名不应阻断: %v", err)
	}
	if len(p.seenTools) == 0 || len(p.seenTools[0]) != 1 || p.seenTools[0][0].Name != "normal" {
		t.Fatalf("清单应只有 normal: %+v", p.seenTools)
	}
}
