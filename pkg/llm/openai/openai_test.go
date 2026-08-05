package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hegq.net/mos/pkg/llm"
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
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
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
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"calculator","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"b\":2}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
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
