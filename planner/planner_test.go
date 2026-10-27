package planner

import (
	"context"
	"errors"
	"testing"

	"github.com/puras/mos/llm"
)

// fakeModel 是可控的模型桩：按预设响应作答，可标记流式能力。
type fakeModel struct {
	resp      *llm.Response
	err       error
	stream    bool
	gotReq    *llm.Request
	streamErr error
}

func (f *fakeModel) Capabilities() llm.Capability {
	if f.stream {
		return llm.CapStreaming
	}
	return 0
}

func (f *fakeModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeModel) ChatStream(_ context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	f.gotReq = req
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	if emit != nil {
		_ = emit(llm.StreamChunk{Delta: f.resp.Message.Content})
	}
	return f.resp, nil
}

func reply(text string) *llm.Response {
	return &llm.Response{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: text},
		Usage:        llm.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		FinishReason: "stop",
	}
}

func TestReActNextAction(t *testing.T) {
	m := &fakeModel{resp: reply("你好")}
	p := NewReAct(m)

	act, err := p.NextAction(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("NextAction: %v", err)
	}
	if act.Content != "你好" || act.HasToolCalls() {
		t.Fatalf("Action = %+v", act)
	}
	if act.Usage.TotalTokens != 10 || act.FinishReason != "stop" {
		t.Fatalf("Action 应透传用量与停止原因: %+v", act)
	}
	if len(m.gotReq.Messages) != 1 {
		t.Fatalf("历史未透传给模型: %+v", m.gotReq)
	}
}

func TestReActToolCallsAndReasoning(t *testing.T) {
	m := &fakeModel{resp: &llm.Response{
		Message: llm.Message{
			Role:             llm.RoleAssistant,
			Content:          "我需要读文件",
			ReasoningContent: "用户要看文件",
			ToolCalls:        []llm.ToolCall{{ID: "c1", Name: "read", Args: `{"path":"a"}`}},
		},
		FinishReason: "tool_calls",
	}}
	act, err := NewReAct(m).NextAction(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("NextAction: %v", err)
	}
	if !act.HasToolCalls() || act.ToolCalls[0].Name != "read" {
		t.Fatalf("工具调用应完整保留: %+v", act)
	}
	if act.ReasoningContent != "用户要看文件" {
		t.Fatalf("思维链应透传: %+v", act)
	}
}

func TestReActStream(t *testing.T) {
	m := &fakeModel{resp: reply("流式回复"), stream: true}
	var got string
	act, err := NewReAct(m).NextActionStream(context.Background(), nil, nil, func(c llm.StreamChunk) error {
		got += c.Delta
		return nil
	})
	if err != nil {
		t.Fatalf("NextActionStream: %v", err)
	}
	if got != "流式回复" {
		t.Fatalf("增量未透出: %q", got)
	}
	if act.Content != "流式回复" {
		t.Fatalf("聚合结果 = %q", act.Content)
	}
}

// 模型声明流式但实现回退 ErrUnsupported：改走非流式并一次性补齐内容。
func TestReActStreamFallbackOnUnsupported(t *testing.T) {
	m := &fakeModel{resp: reply("回退内容"), stream: true, streamErr: llm.ErrUnsupported}
	var got string
	act, err := NewReAct(m).NextActionStream(context.Background(), nil, nil, func(c llm.StreamChunk) error {
		got += c.Delta
		return nil
	})
	if err != nil {
		t.Fatalf("应回退而非报错: %v", err)
	}
	if got != "回退内容" || act.Content != "回退内容" {
		t.Fatalf("回退路径内容不一致: emit=%q act=%q", got, act.Content)
	}
}

// 未声明流式能力：直接走非流式，不尝试 ChatStream。
func TestReActStreamSkipsWhenCapabilityAbsent(t *testing.T) {
	m := &fakeModel{resp: reply("非流式")}
	act, err := NewReAct(m).NextActionStream(context.Background(), nil, nil, nil)
	if err != nil || act.Content != "非流式" {
		t.Fatalf("act=%+v err=%v", act, err)
	}
}

func TestReActStreamRealErrorPropagates(t *testing.T) {
	boom := errors.New("network down")
	m := &fakeModel{resp: reply("x"), stream: true, streamErr: boom}
	if _, err := NewReAct(m).NextActionStream(context.Background(), nil, nil, nil); !errors.Is(err, boom) {
		t.Fatalf("非 ErrUnsupported 错误应冒泡, got %v", err)
	}
}

func TestReActChatError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := NewReAct(&fakeModel{err: boom}).NextAction(context.Background(), nil, nil); !errors.Is(err, boom) {
		t.Fatalf("错误应冒泡, got %v", err)
	}
}
