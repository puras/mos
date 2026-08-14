package planner

import (
	"context"
	"errors"
	"testing"

	"hegq.net/mos/llm"
)

// mockChatModel 是可编程的 ChatModel（测试 ReAct 决策器）。
type mockChatModel struct {
	cap        llm.Capability
	resp       *llm.Response
	err        error
	streamResp *llm.Response
	streamErr  error
	chunks     []llm.StreamChunk
}

func (m *mockChatModel) Capabilities() llm.Capability { return m.cap }
func (m *mockChatModel) Chat(context.Context, *llm.Request) (*llm.Response, error) {
	return m.resp, m.err
}
func (m *mockChatModel) ChatStream(_ context.Context, _ *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	for _, c := range m.chunks {
		if err := emit(c); err != nil {
			return nil, err
		}
	}
	return m.streamResp, nil
}

func TestReActNextActionToolCall(t *testing.T) {
	m := &mockChatModel{
		resp: &llm.Response{
			Message: llm.Message{
				Role:    llm.RoleAssistant,
				Content: "我来调用工具",
				ToolCalls: []llm.ToolCall{
					{ID: "c1", Name: "calculator", Args: `{"a":1}`},
					{ID: "c2", Name: "grep", Args: `{"p":"x"}`},
				},
			},
			FinishReason: "tool_calls",
		},
	}
	p := NewReAct(m)
	action, err := p.NextAction(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("NextAction: %v", err)
	}
	if action.ToolCall == nil || action.ToolCall.ID != "c1" {
		t.Fatalf("ToolCall 应指向第一个调用: %+v", action.ToolCall)
	}
	if len(action.ToolCalls) != 2 {
		t.Fatalf("ToolCalls 应完整保留（多调用），实际 %d", len(action.ToolCalls))
	}
	if action.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason 应透传: %q", action.FinishReason)
	}
}

func TestReActNextActionDirectAnswer(t *testing.T) {
	m := &mockChatModel{resp: &llm.Response{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: "完成"},
		FinishReason: "stop",
	}}
	action, err := NewReAct(m).NextAction(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("NextAction: %v", err)
	}
	if action.ToolCall != nil || len(action.ToolCalls) != 0 {
		t.Fatalf("无工具调用时应直接作答: %+v", action)
	}
	if action.Content != "完成" || action.FinishReason != "stop" {
		t.Fatalf("content/finish 透传错误: %+v", action)
	}
}

func TestReActNextActionError(t *testing.T) {
	m := &mockChatModel{err: errors.New("boom")}
	if _, err := NewReAct(m).NextAction(context.Background(), nil, nil); err == nil {
		t.Fatal("模型错误应透传")
	}
}

func TestReActNextActionStream(t *testing.T) {
	m := &mockChatModel{
		cap: llm.CapStreaming | llm.CapFunctionCalling,
		chunks: []llm.StreamChunk{
			{Delta: "你好"},
			{Delta: "世界"},
			{FinishReason: "stop"},
		},
		streamResp: &llm.Response{
			Message:      llm.Message{Role: llm.RoleAssistant, Content: "你好世界"},
			FinishReason: "stop",
		},
	}
	var deltas []string
	action, err := NewReAct(m).NextActionStream(context.Background(), nil, nil, func(c llm.StreamChunk) error {
		if c.Delta != "" {
			deltas = append(deltas, c.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("NextActionStream: %v", err)
	}
	if action.Content != "你好世界" {
		t.Fatalf("content = %q", action.Content)
	}
	if len(deltas) != 2 || deltas[0] != "你好" || deltas[1] != "世界" {
		t.Fatalf("流式增量应按序透传: %v", deltas)
	}
}

func TestReActNextActionStreamFallback(t *testing.T) {
	// 声明流式但实现返回 ErrUnsupported → 回退非流式，emit 一次性补齐完整内容
	m := &mockChatModel{
		cap:       llm.CapStreaming,
		streamErr: llm.ErrUnsupported,
		resp: &llm.Response{
			Message:      llm.Message{Role: llm.RoleAssistant, Content: "回退完成"},
			FinishReason: "stop",
		},
	}
	var deltas []string
	action, err := NewReAct(m).NextActionStream(context.Background(), nil, nil, func(c llm.StreamChunk) error {
		if c.Delta != "" {
			deltas = append(deltas, c.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("NextActionStream fallback: %v", err)
	}
	if action.Content != "回退完成" {
		t.Fatalf("content = %q", action.Content)
	}
	if len(deltas) != 1 || deltas[0] != "回退完成" {
		t.Fatalf("回退应一次性补齐完整内容: %v", deltas)
	}
}

func TestToActionTruncated(t *testing.T) {
	// 截断响应：FinishReason=length + 工具调用 → 循环层据此 fail-closed（见 agent 测试）
	action := toAction(&llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   "截断",
			ToolCalls: []llm.ToolCall{{ID: "c1", Name: "calculator", Args: `{}`}},
		},
		FinishReason: "length",
		Usage:        llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	})
	if action.FinishReason != "length" {
		t.Fatalf("FinishReason = %q", action.FinishReason)
	}
	if action.ToolCall == nil || len(action.ToolCalls) != 1 {
		t.Fatalf("ToolCall/ToolCalls 透传错误: %+v", action)
	}
	if action.Usage.TotalTokens != 15 {
		t.Fatalf("Usage 透传错误: %+v", action.Usage)
	}
}
