package anthropic

import (
	"context"
	"encoding/json"
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
		"model":    "claude-3-5-sonnet",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*Model), srv
}

func TestChatWithToolUse(t *testing.T) {
	var gotKey, gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"content":[
				{"type":"text","text":"结果是 3"},
				{"type":"tool_use","id":"toolu_2","name":"calculator","input":{"a":3,"b":4}}
			],
			"usage":{"input_tokens":10,"output_tokens":5}
		}`)
	})
	defer srv.Close()

	resp, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "你是助手"},
			{Role: llm.RoleUser, Content: "1+2=?"},
			{Role: llm.RoleAssistant, Content: "我来算", ToolCalls: []llm.ToolCall{{ID: "toolu_1", Name: "calculator", Args: `{"a":1,"b":2}`}}},
			{Role: llm.RoleTool, Content: "3", ToolCallID: "toolu_1", Name: "calculator"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotKey != "test-key" {
		t.Fatalf("x-api-key = %q", gotKey)
	}

	// 验证 system 提取与工具结果块结构
	var body struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				ToolUseID string `json:"tool_use_id"`
				Content   string `json:"content"`
			} `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.System != "你是助手" {
		t.Fatalf("system = %q", body.System)
	}
	if body.MaxTokens != defaultMaxTokens {
		t.Fatalf("max_tokens = %d", body.MaxTokens)
	}
	last := body.Messages[len(body.Messages)-1]
	if last.Role != "user" || len(last.Content) != 1 || last.Content[0].Type != "tool_result" ||
		last.Content[0].ToolUseID != "toolu_1" || last.Content[0].Content != "3" {
		t.Fatalf("last message = %+v", last)
	}

	if resp.Message.Content != "结果是 3" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_2" || tc.Name != "calculator" || tc.Args != `{"a":3,"b":4}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestChatStream(t *testing.T) {
	sse := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
	}, "\n")

	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	defer srv.Close()

	var deltas []string
	var finish string
	resp, err := p.ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(c llm.StreamChunk) error {
		if c.Delta != "" {
			deltas = append(deltas, c.Delta)
		}
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Message.Content != "Hi there" {
		t.Fatalf("content = %q", resp.Message.Content)
	}
	if strings.Join(deltas, "") != "Hi there" {
		t.Fatalf("deltas = %v", deltas)
	}
	if finish != "end_turn" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestChatStreamToolUse(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"calculator","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":",\"b\":2}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}, "\n")

	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	defer srv.Close()

	var emitted []llm.ToolCall
	resp, err := p.ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "1+2"}},
	}, func(c llm.StreamChunk) error {
		if c.ToolCall != nil {
			emitted = append(emitted, *c.ToolCall)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "calculator" || tc.Args != `{"a":1,"b":2}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted = %+v", emitted)
	}
}

func TestNewMissingConfig(t *testing.T) {
	if _, err := New(llm.Config{"model": "claude-3"}); err == nil {
		t.Fatal("New without api_key should fail")
	}
}

func TestChatUsageCacheDetails(t *testing.T) {
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"content":[{"type":"text","text":"ok"}],
			"usage":{"input_tokens":200,"output_tokens":50,
				"cache_creation_input_tokens":120,"cache_read_input_tokens":80}
		}`)
	})
	defer srv.Close()

	resp, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	d := resp.Usage.Details.Input
	if d.CacheWrite != 120 || d.CacheRead != 80 {
		t.Fatalf("cache details = %+v", d)
	}
	if resp.Usage.TotalTokens != 250 {
		t.Fatalf("total = %d", resp.Usage.TotalTokens)
	}
}

func TestChatContentPartsImage(t *testing.T) {
	var gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{}}`)
	})
	defer srv.Close()

	_, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			ContentParts: []llm.ContentPart{
				{Type: "text", Text: "看图"},
				{Type: "image", URL: "https://example.com/a.png", MIME: "image/png"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := `"content":[{"type":"text","text":"看图"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]`
	if !strings.Contains(gotBody, want) {
		t.Fatalf("request body missing image block:\n%s", gotBody)
	}
}
