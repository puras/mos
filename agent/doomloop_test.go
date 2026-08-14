package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

func TestDoomLoopDetect(t *testing.T) {
	d := newDoomLoopDetector(3)
	call := &llm.ToolCall{Name: "grep", Args: `{"p":"x"}`}
	if d.observe(call) {
		t.Fatalf("第 1 次不应判定")
	}
	if d.observe(call) {
		t.Fatalf("第 2 次不应判定")
	}
	if !d.observe(call) {
		t.Fatalf("第 3 次相同调用应判定 Doom Loop")
	}
}

func TestDoomLoopDetectResetsOnDiffArgs(t *testing.T) {
	d := newDoomLoopDetector(3)
	// 连续 2 次 x：未判定
	if d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"x"}`}) {
		t.Fatalf("x 第 1 次不应判定")
	}
	if d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"x"}`}) {
		t.Fatalf("x 第 2 次不应判定")
	}
	// 参数变化重置计数：y 第 1、2 次均不判定
	if d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"y"}`}) {
		t.Fatalf("y 第 1 次（参数变化重置）不应判定")
	}
	if d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"y"}`}) {
		t.Fatalf("y 第 2 次不应判定")
	}
	// y 第 3 次连续相同 → 判定
	if !d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"y"}`}) {
		t.Fatalf("y 第 3 次连续相同应判定")
	}
}

// doomModel 每轮都返回相同的工具调用（死循环模式）。
type doomModel struct{ n int }

func (m *doomModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *doomModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	m.n++
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "再试一次",
		ToolCalls: []llm.ToolCall{{ID: fmt.Sprintf("c%d", m.n), Name: "calculator", Args: `{"a":1,"b":2}`}},
	}}, nil
}
func (m *doomModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

func TestDoomLoopInAgent(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := New(&RunContext{
		Tools:             reg,
		Planner:           planner.NewReAct(&doomModel{}),
		DoomLoopMaxRepeat: 3,
		MaxTurns:          10,
	})

	sess := NewSession("s1")
	_, err := a.Run(context.Background(), sess, "反复执行")
	if err == nil || !strings.Contains(err.Error(), "doom loop") {
		t.Fatalf("应中断为 doom loop 错误: %v", err)
	}
	if sess.State.Status != StatusBlocked {
		t.Fatalf("Doom Loop 后状态应 blocked: %s", sess.State.Status)
	}
}

// usageModel 返回带 token 用量的响应。
type usageModel struct{}

func (usageModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (usageModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == llm.RoleTool {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "done"}, Usage: llm.Usage{PromptTokens: 6000, TotalTokens: 6100}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "调用",
		ToolCalls: []llm.ToolCall{{ID: "c1", Name: "calculator", Args: `{"a":1,"b":2}`}},
	}, Usage: llm.Usage{PromptTokens: 5000, TotalTokens: 5010}}, nil
}
func (usageModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

func TestContextUsageMonitoring(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(calculator{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	var usageEvents []Event
	a := New(&RunContext{
		Tools:         reg,
		Planner:       planner.NewReAct(usageModel{}),
		ContextBudget: 10000,
		Observer: func(ev Event) {
			if ev.Type == EventContextUsage {
				usageEvents = append(usageEvents, ev)
			}
		},
	})

	sess := NewSession("s1")
	if _, err := a.Run(context.Background(), sess, "统计用量"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(usageEvents) != 2 {
		t.Fatalf("应有 2 次用量事件（两轮各一次），实际 %d", len(usageEvents))
	}
	if usageEvents[0].Content != "50%" || usageEvents[1].Content != "60%" {
		t.Fatalf("利用率计算错误: %+v %+v", usageEvents[0].Content, usageEvents[1].Content)
	}
	if usageEvents[0].Usage.PromptTokens != 5000 {
		t.Fatalf("用量事件应携带 prompt tokens: %+v", usageEvents[0].Usage)
	}
}
