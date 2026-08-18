package scripted

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// fakePlanner 是固定脚本的 ScriptPlanner（不依赖模型），供测试确定性编排。
type fakePlanner struct{ plan planner.ScriptedPlan }

func (f *fakePlanner) Script(context.Context, string, []llm.Message, []llm.Tool) (planner.ScriptedPlan, error) {
	return f.plan, nil
}

// synthModel 是合成轮用的假模型：固定返回一段文本。
type synthModel struct{ reply string }

func (m *synthModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Content: m.reply}}, nil
}
func (m *synthModel) ChatStream(_ context.Context, _ *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(context.Background(), nil)
}
func (m *synthModel) Capabilities() llm.Capability { return 0 }

// toolCallModel 是合成轮返回工具调用的假模型（触发 fail-closed）。
type toolCallModel struct{}

func (m *toolCallModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{
		Content:   "",
		ToolCalls: []llm.ToolCall{{ID: "t1", Name: "t1", Args: "{}"}},
	}}, nil
}
func (m *toolCallModel) ChatStream(_ context.Context, _ *llm.Request, _ func(llm.StreamChunk) error) (*llm.Response, error) {
	return m.Chat(context.Background(), nil)
}
func (m *toolCallModel) Capabilities() llm.Capability { return 0 }

// newSession 是测试固定 ID 的会话创建助手。
func newSession(id string) *session.Session { return session.NewSession(id) }

// TestScriptedDriverEndToEnd 验证 ScriptedDriver 完整流程：脚本生成 → 分波执行 →
// 合成回复 → 日志完整性（script/start + script/end + tool/call + tool/result + turn/end）。
func TestScriptedDriverEndToEnd(t *testing.T) {
	reg := tool.NewRegistry()
	for _, tl := range []tool.Tool{
		orderTool{name: "t1"}, orderTool{name: "t2"},
	} {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	sess := newSession("s")
	rt := &driver.RunContext{
		Tools:   reg,
		Planner: planner.NewReAct(&synthModel{reply: "完成，共处理了 2 步。"}),
		ScriptPlanner: &fakePlanner{plan: planner.ScriptedPlan{
			Goal: "g",
			Steps: []planner.ScriptStep{
				{ID: "a", Tool: "t1"},
				{ID: "b", Tool: "t2", DependsOn: []string{"a"}},
			},
		}},
	}
	ag := New(rt)
	reply, err := ag.Run(context.Background(), sess, "g")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(reply, "完成") {
		t.Fatalf("unexpected reply: %q", reply)
	}
	// 日志完整性：两个工具调用都落了 tool/call + tool/result，且 script/end 收尾
	logs := eventTypes(sess)
	for _, want := range []string{"script/start", "script/end", "tool/call", "tool/result", "turn/end"} {
		if !containsStr(logs, want) {
			t.Fatalf("log missing %q: %v", want, logs)
		}
	}
	// script/end 载荷含执行报告：每步状态 + 命名输出摘要（架构 v2 §6 契约）
	var scriptEnd event.ScriptEndData
	for _, ev := range sess.Events {
		if ev.Type != "script/end" {
			continue
		}
		if err := json.Unmarshal(ev.Data, &scriptEnd); err != nil {
			t.Fatalf("unmarshal script/end: %v", err)
		}
		break
	}
	if scriptEnd.Steps["a"] != "done" || scriptEnd.Steps["b"] != "done" {
		t.Fatalf("script/end Steps = %v, want a=done, b=done", scriptEnd.Steps)
	}
	if got := scriptEnd.Outputs["a"]; got != "result-t1" {
		t.Fatalf("script/end Outputs[a] = %#v, want result-t1", got)
	}
	if got := scriptEnd.Outputs["b"]; got != "result-t2" {
		t.Fatalf("script/end Outputs[b] = %#v, want result-t2", got)
	}
}

// TestScriptedDriverRejectsNoPlanner 验证 fail-closed：未配置 ScriptPlanner 时驱动拒绝运行。
func TestScriptedDriverRejectsNoPlanner(t *testing.T) {
	rt := &driver.RunContext{Tools: tool.NewRegistry()}
	ag := New(rt)
	_, err := ag.Run(context.Background(), newSession("s"), "g")
	if err == nil {
		t.Fatal("expected error when ScriptPlanner missing")
	}
}

// TestScriptedDriverSynthesisToolCallsFailClosed 验证合成轮返回工具调用 → fail-closed
// （不执行工具调用，记失败回填 + 状态 blocked）。
func TestScriptedDriverSynthesisToolCallsFailClosed(t *testing.T) {
	reg := tool.NewRegistry()
	_ = reg.Register(orderTool{name: "t1"})
	sess := newSession("s")
	rt := &driver.RunContext{
		Tools:   reg,
		Planner: planner.NewReAct(&toolCallModel{}),
		ScriptPlanner: &fakePlanner{plan: planner.ScriptedPlan{
			Goal:  "g",
			Steps: []planner.ScriptStep{{ID: "a", Tool: "t1"}},
		}},
	}
	ag := New(rt)
	if _, err := ag.Run(context.Background(), sess, "g"); err == nil {
		t.Fatal("expected error when synthesis returns tool calls")
	}
}

// 辅助：记录日志事件类型 / 判断包含。
func eventTypes(sess *session.Session) []string {
	out := make([]string, 0, len(sess.Events))
	for _, ev := range sess.Events {
		out = append(out, ev.Type)
	}
	return out
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
