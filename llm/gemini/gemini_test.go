package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/puras/mos/llm"
)

func newTestModel(t *testing.T, handler http.HandlerFunc) (*Chat, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	p, err := New(llm.Config{
		"api_key":  "test-key",
		"base_url": srv.URL,
		"model":    "gemini-2.5-flash",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*Chat), srv
}

func TestChatWithToolCall(t *testing.T) {
	var gotKey, gotPath, gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"candidates":[{
				"content":{"role":"model","parts":[
					{"text":"我来算"},
					{"functionCall":{"id":"fc_1","name":"calculator","args":{"a":1,"b":2}}}
				]},
				"finishReason":"STOP"
			}],
			"usageMetadata":{
				"promptTokenCount":30,"candidatesTokenCount":8,"totalTokenCount":38,
				"cachedContentTokenCount":10,"thoughtsTokenCount":5
			}
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
	if gotKey != "test-key" {
		t.Fatalf("x-goog-api-key = %q", gotKey)
	}
	if !strings.HasSuffix(gotPath, "/models/gemini-2.5-flash:generateContent") {
		t.Fatalf("path = %q", gotPath)
	}
	for _, want := range []string{
		`"functionDeclarations":[{"name":"calculator","description":"计算器","parameters":{"type":"object"}}]`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("request body missing %s:\n%s", want, gotBody)
		}
	}

	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "fc_1" || tc.Name != "calculator" || tc.Args != `{"a":1,"b":2}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if resp.Usage.TotalTokens != 38 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	d := resp.Usage.Details
	if d.Input.CacheRead != 10 {
		t.Fatalf("cache read = %d", d.Input.CacheRead)
	}
	if d.Output.Reasoning != 5 {
		t.Fatalf("reasoning = %d", d.Output.Reasoning)
	}
}

func TestChatStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":""}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":" world"}]},"finishReason":"STOP"}]}`,
		`data: {"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`,
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
	if resp.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestChatStreamToolCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"fc_1","name":"calculator","args":{}}}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"fc_1","name":"calculator","args":{"a":1,"b":2}}}]},"finishReason":"STOP"}]}`,
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
	if tc.ID != "fc_1" || tc.Name != "calculator" {
		t.Fatalf("tool call = %+v", tc)
	}
}

func TestChatContentPartsAndStructuredOutput(t *testing.T) {
	var gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	})
	defer srv.Close()

	_, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			ContentParts: []llm.ContentPart{
				{Type: "text", Text: "描述"},
				{Type: "image", Data: "aGVsbG8=", MIME: "image/png"},
			},
		}},
		ResponseFormat: &llm.ResponseFormat{
			Type: "json_schema", Name: "answer",
			Schema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
		},
		MaxTokens: intPtr(64),
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, want := range []string{
		`"contents":[{"parts":[{"text":"描述"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}],"role":"user"}]`,
		`"responseMimeType":"application/json"`,
		`"responseSchema":{"type":"object","properties":{"ok":{"type":"boolean"}}}`,
		`"maxOutputTokens":64`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("request body missing %s:\n%s", want, gotBody)
		}
	}
}

func TestChatToolResultRoundTrip(t *testing.T) {
	var gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"3"}]},"finishReason":"STOP"}]}`)
	})
	defer srv.Close()

	_, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "1+2?"},
			{Role: llm.RoleAssistant, Content: "", ToolCalls: []llm.ToolCall{{ID: "fc_1", Name: "calculator", Args: `{"a":1,"b":2}`}}},
			{Role: llm.RoleTool, Content: "3", ToolCallID: "fc_1", Name: "calculator"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := `"functionResponse":{"id":"fc_1","name":"calculator","response":{"result":"3"}}`
	if !strings.Contains(gotBody, want) {
		t.Fatalf("request body missing functionResponse:\n%s", gotBody)
	}
}

func TestNewMissingConfig(t *testing.T) {
	if _, err := New(llm.Config{"model": "gemini-2.5-flash"}); err == nil {
		t.Fatal("New without api_key should fail")
	}
}

func TestErrorResponse(t *testing.T) {
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":401,"message":"API key not valid.","status":"INVALID_ARGUMENT"}}`)
	})
	defer srv.Close()

	_, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("err = %v", err)
	}
}

func intPtr(n int) *int { return &n }

// 流被掐断（没收到 finishReason 就结束）必须报错，不能把半截聚合当正常响应。
func TestChatStreamTruncatedErrors(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"半截回答"}]}}]}`,
		// ← 没有 finishReason 块：连接被掐
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer srv.Close()

	p, err := New(llm.Config{"api_key": "k", "model": "gemini-pro", "base_url": srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.(*Chat).ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(c llm.StreamChunk) error { return nil })
	if !errors.Is(err, llm.ErrStreamTruncated) {
		t.Fatalf("应返回 ErrStreamTruncated, got %v", err)
	}
}

// require_finish=false：宿主用不报 finishReason 的网关时的逃生阀。
func TestRequireFinishFalseAllowsMissingFinish(t *testing.T) {
	sse := `data: {"candidates":[{"content":{"parts":[{"text":"回答"}]}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer srv.Close()

	p, err := New(llm.Config{"api_key": "k", "model": "m", "base_url": srv.URL, "require_finish": "false"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.(*Chat).ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(c llm.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("关闭检查后应放行: %v", err)
	}
	if resp.Message.Content != "回答" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
}
