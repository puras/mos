package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hegq.net/mos/llm"
)

func newTestModel(t *testing.T, handler http.HandlerFunc) (*Model, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	p, err := New(llm.Config{
		"api_key":  "test-key",
		"base_url": srv.URL,
		"model":    "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*Model), srv
}

func TestChatWithToolCall(t *testing.T) {
	var gotAuth, gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"choices":[{"index":0,"message":{"role":"assistant","content":"我来计算",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"calculator","arguments":"{\"a\":1,\"b\":2}"}}]},
				"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`)
	})
	defer srv.Close()

	resp, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "1+2=?"}},
		Tools:    []llm.Tool{{Name: "calculator", Description: "计算器", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"gpt-4o-mini"`) ||
		!strings.Contains(gotBody, `"name":"calculator"`) {
		t.Fatalf("request body = %s", gotBody)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "calculator" || tc.Args != `{"a":1,"b":2}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestChatStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	defer srv.Close()

	var deltas []string
	resp, err := p.ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(c llm.StreamChunk) error {
		if c.Delta != "" {
			deltas = append(deltas, c.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Message.Content != "Hello world" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
	if strings.Join(deltas, "") != "Hello world" {
		t.Fatalf("deltas = %v", deltas)
	}
}

func TestChatStreamToolCallAggregation(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"calculator","arguments":""}}]}}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1"}}]}}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"b\":2}"}}]}}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	defer srv.Close()

	resp, err := p.ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "1+2"}},
	}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "calculator" || tc.Args != `{"a":1,"b":2}` {
		t.Fatalf("tool call = %+v", tc)
	}
}

func TestNewMissingModel(t *testing.T) {
	if _, err := New(llm.Config{}); err == nil {
		t.Fatal("New without model should fail")
	}
}

func TestRegistryFactory(t *testing.T) {
	reg := llm.NewRegistry()
	reg.Register("openai", New)
	p, err := reg.Create("openai", llm.Config{"model": "gpt-4o-mini", "base_url": "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !p.Capabilities().HasCapability(llm.CapFunctionCalling) {
		t.Fatal("expected function calling capability")
	}
	if _, err := reg.Create("nope", nil); err == nil {
		t.Fatal("Create unknown provider should fail")
	}
}

func TestChatUsageDetails(t *testing.T) {
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{
				"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,
				"prompt_tokens_details":{"cached_tokens":60,"audio_tokens":0},
				"completion_tokens_details":{"reasoning_tokens":10,"audio_tokens":5}
			}
		}`)
	})
	defer srv.Close()

	resp, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	d := resp.Usage.Details
	if d.Input.CacheRead != 60 {
		t.Fatalf("cache read = %d", d.Input.CacheRead)
	}
	if d.Output.Reasoning != 10 {
		t.Fatalf("reasoning = %d", d.Output.Reasoning)
	}
	if d.Output.Audio != 5 {
		t.Fatalf("audio out = %d", d.Output.Audio)
	}
}

func TestChatContentPartsAndStructuredOutput(t *testing.T) {
	var gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{}}`)
	})
	defer srv.Close()

	effort := "medium"
	_, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			ContentParts: []llm.ContentPart{
				{Type: "text", Text: "描述这张图"},
				{Type: "image", URL: "https://example.com/a.png", MIME: "image/png"},
			},
		}},
		ResponseFormat:  &llm.ResponseFormat{Type: "json_schema", Name: "answer", Schema: map[string]any{"type": "object"}},
		Modalities:      []string{"text"},
		ReasoningEffort: &effort,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, want := range []string{
		`"content":[{"type":"text","text":"描述这张图"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`,
		`"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}`,
		`"modalities":["text"]`,
		`"reasoning_effort":"medium"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("request body missing %s:\n%s", want, gotBody)
		}
	}
}

func TestChatReasoningContentRoundTrip(t *testing.T) {
	var gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"choices":[{"index":0,"message":{"role":"assistant","content":"答案","reasoning_content":"先思考再回答"},"finish_reason":"stop"}],
			"usage":{}
		}`)
	})
	defer srv.Close()

	// 历史中包含带推理内容的 assistant 消息（thinking 模式多轮需原样回传）
	_, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "1+2=?"},
			{Role: llm.RoleAssistant, Content: "我来计算", ReasoningContent: "先思考再回答"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(gotBody, `"reasoning_content":"先思考再回答"`) {
		t.Fatalf("request body missing reasoning_content:\n%s", gotBody)
	}

	// 响应侧解析
	resp, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.ReasoningContent != "先思考再回答" {
		t.Fatalf("reasoning content = %q", resp.Message.ReasoningContent)
	}
	if resp.Message.Content != "答案" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
}

func TestChatStreamReasoningContent(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"先"}}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"答案","reasoning_content":"思考"}}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"再回答"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	defer srv.Close()

	resp, err := p.ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Message.ReasoningContent != "先思考再回答" {
		t.Fatalf("reasoning content = %q", resp.Message.ReasoningContent)
	}
	if resp.Message.Content != "答案" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
}

func TestChatStreamUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13,"completion_tokens_details":{"reasoning_tokens":2}}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	defer srv.Close()

	resp, err := p.ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.Usage.Details.Output.Reasoning != 2 {
		t.Fatalf("reasoning = %+v", resp.Usage.Details)
	}
}
