package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"hegq.net/mos/llm"
	"hegq.net/mos/tool"
)

// mockProvider 模拟：首轮返回工具调用，收到工具结果后返回最终答案。
type mockProvider struct{}

func (mockProvider) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (mockProvider) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleTool {
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "结果是 " + last.Content,
		}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "我来调用工具",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "calculator", Args: `{"a":1,"b":2}`}},
	}}, nil
}
func (mockProvider) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

type calcParams struct {
	A int `json:"a" mos:"desc=加数"`
	B int `json:"b" mos:"desc=被加数"`
}

type calculator struct{}

func (calculator) Name() string        { return "calculator" }
func (calculator) Description() string { return "计算两个整数之和" }
func (calculator) Parameters() any     { return &calcParams{} }
func (calculator) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[calcParams](args)
	if err != nil {
		return nil, err
	}
	return p.A + p.B, nil
}

func TestAgentReActLoop(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	var events []Event
	a := New(mockProvider{}, reg,
		WithObserver(func(ev Event) { events = append(events, ev) }),
		WithSystemPrompt("你是助手"),
	)

	sess := NewSession("s1")
	reply, err := a.Run(context.Background(), sess, "1+2=3?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "结果是 3" {
		t.Fatalf("reply = %q", reply)
	}

	// 历史应为：user, assistant(工具调用), tool(结果), assistant(最终)
	if len(sess.History) != 4 {
		t.Fatalf("history len = %d, want 4: %+v", len(sess.History), sess.History)
	}
	if sess.History[1].Role != llm.RoleAssistant || len(sess.History[1].ToolCalls) != 1 {
		t.Fatalf("history[1] = %+v", sess.History[1])
	}
	if sess.History[2].Role != llm.RoleTool || sess.History[2].Content != "3" {
		t.Fatalf("history[2] = %+v", sess.History[2])
	}

	// 事件序列：思考 → 工具调用 → 工具结果 → 思考 → 完成
	var gotTypes []EventType
	for _, ev := range events {
		gotTypes = append(gotTypes, ev.Type)
	}
	want := []EventType{EventStep, EventToolCall, EventToolResult, EventStep, EventDone}
	if len(gotTypes) != len(want) {
		t.Fatalf("events = %v", gotTypes)
	}
	for i := range want {
		if gotTypes[i] != want[i] {
			t.Fatalf("events = %v, want %v", gotTypes, want)
		}
	}
	if events[len(events)-1].Iteration != 2 {
		t.Fatalf("final iteration = %d, want 2", events[len(events)-1].Iteration)
	}
}

// denyPolicy 拒绝所有工具调用。
type denyPolicy struct{}

func (denyPolicy) Allow(_ context.Context, call *llm.ToolCall) error {
	return fmt.Errorf("工具 %s 未授权", call.Name)
}

func TestAgentPolicyDenial(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := New(mockProvider{}, reg, WithPolicy(denyPolicy{}))

	sess := NewSession("s1")
	reply, err := a.Run(context.Background(), sess, "1+2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// mock 会将工具（拒绝）结果原样返回，应包含拒绝原因
	if !strings.Contains(reply, "未授权") {
		t.Fatalf("reply = %q, want policy denial", reply)
	}
	// 工具未实际执行：history[2] 是拒绝反馈
	if len(sess.History) != 4 {
		t.Fatalf("history len = %d", len(sess.History))
	}
	if !strings.Contains(sess.History[2].Content, "未授权") {
		t.Fatalf("history[2] = %+v", sess.History[2])
	}
}

// 多轮对话应保留历史，且 system 引导不进入会话历史。
func TestAgentMultiTurn(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := New(mockProvider{}, reg, WithSystemPrompt("你是助手"))
	sess := NewSession("s1")

	if _, err := a.Run(context.Background(), sess, "第一轮"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := a.Run(context.Background(), sess, "第二轮"); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	for _, m := range sess.History {
		if m.Role == llm.RoleSystem {
			t.Fatal("system message should not persist in session history")
		}
	}
	if len(sess.History) != 8 {
		t.Fatalf("history len = %d, want 8 (两轮各 4 条)", len(sess.History))
	}
}
