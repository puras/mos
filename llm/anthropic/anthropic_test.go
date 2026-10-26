package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/puras/mos/llm"
)

func newTestModel(t *testing.T, handler http.HandlerFunc) (*Chat, *httptest.Server) {
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
	return p.(*Chat), srv
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
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":21}}`,
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
	// moproto 口径：TotalTokens = 全上下文大小 =
	// input + output + cache 写入 + cache 读取 = 200+50+120+80。
	if resp.Usage.TotalTokens != 450 {
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

func TestChatThinkingRequest(t *testing.T) {
	var gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	defer srv.Close()

	effort := "high"
	_, err := p.Chat(context.Background(), &llm.Request{
		Messages:        []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		ReasoningEffort: &effort,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// high → budget 16384；起点 128000 > budget，无需提升。
	if !strings.Contains(gotBody, `"thinking":{"type":"enabled","budget_tokens":16384}`) {
		t.Fatalf("request body missing thinking config:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, `"max_tokens":128000`) {
		t.Fatalf("max_tokens should stay above budget:\n%s", gotBody)
	}

	// 显式 max_tokens 已高于 budget 时保持不动。
	gotBody = ""
	maxTokens := 32768
	_, err = p.Chat(context.Background(), &llm.Request{
		Messages:        []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		ReasoningEffort: &effort,
		MaxTokens:       &maxTokens,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(gotBody, `"max_tokens":32768`) {
		t.Fatalf("explicit max_tokens should be kept:\n%s", gotBody)
	}
}

func TestChatThinkingResponse(t *testing.T) {
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"content":[
				{"type":"thinking","thinking":"先想想…","signature":"sig"},
				{"type":"text","text":"答案是 42"}
			],
			"usage":{"input_tokens":10,"output_tokens":5}
		}`)
	})
	defer srv.Close()

	resp, err := p.Chat(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.ReasoningContent != "先想想…" {
		t.Fatalf("ReasoningContent = %q", resp.Message.ReasoningContent)
	}
	if resp.Message.Content != "答案是 42" {
		t.Fatalf("Content = %q", resp.Message.Content)
	}
}

func TestChatStreamThinking(t *testing.T) {
	sse := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想想…"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案是 42"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
	}, "\n")

	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	defer srv.Close()

	var reasoningDeltas []string
	resp, err := p.ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(c llm.StreamChunk) error {
		if c.Reasoning != "" {
			reasoningDeltas = append(reasoningDeltas, c.Reasoning)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Message.ReasoningContent != "先想想…" {
		t.Fatalf("ReasoningContent = %q", resp.Message.ReasoningContent)
	}
	if resp.Message.Content != "答案是 42" {
		t.Fatalf("Content = %q", resp.Message.Content)
	}
	if strings.Join(reasoningDeltas, "") != "先想想…" {
		t.Fatalf("reasoning deltas = %v", reasoningDeltas)
	}
}

// 流被掐断（没收到 stop_reason 就结束）必须报错，不能把半截聚合当正常响应。
// （2026-10 事故：半截文本 + 空 args 的 write_file 被当成完整消息处理。）
func TestChatStreamTruncatedErrors(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"role":"assistant"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半截回答"}}`,
		// ← 没有 message_delta / message_stop：连接被掐
	}, "\n")

	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	defer srv.Close()

	_, err := p.ChatStream(context.Background(), &llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(c llm.StreamChunk) error { return nil })
	if !errors.Is(err, llm.ErrStreamTruncated) {
		t.Fatalf("应返回 ErrStreamTruncated, got %v", err)
	}
}

// max_tokens 可经 Config 配置（宿主的成本旋钮）。
func TestMaxTokensFromConfig(t *testing.T) {
	var gotBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	defer srv.Close()

	if _, err := p.Chat(context.Background(), &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// 未配置 → 起点 128000（撞到模型上限会自适应降档，见 TestMaxTokensAdaptiveDowngrade）
	if !strings.Contains(gotBody, `"max_tokens":128000`) {
		t.Fatalf("默认 max_tokens 应为 128000:\n%s", gotBody)
	}
}

// 自适应降档：起点 128000 超模型上限 → 400（信息含 max_tokens）→ 对半重试
// → 命中后按工作值继续；宿主显式配置时不降档（配置错了就大声失败）。
func TestMaxTokensAdaptiveDowngrade(t *testing.T) {
	var calls int32
	var secondBody string
	p, srv := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 128000 > 64000, which is the maximum allowed number of output tokens"}}`)
			return
		}
		secondBody = string(b)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	defer srv.Close()

	resp, err := p.Chat(context.Background(), &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("降档后应成功: %v", err)
	}
	if resp == nil || resp.Message.Content != "ok" {
		t.Fatalf("resp = %+v", resp)
	}
	if !strings.Contains(secondBody, `"max_tokens":64000`) {
		t.Fatalf("重试应对半降到 64000:\n%s", secondBody)
	}

	// 探测值被记住：后续请求直接用 64000，不再撞 400
	calls0 := atomic.LoadInt32(&calls)
	if _, err := p.Chat(context.Background(), &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "again"}}}); err != nil {
		t.Fatalf("后续请求: %v", err)
	}
	if atomic.LoadInt32(&calls) != calls0+1 {
		t.Fatal("记住探测值后不应再撞 400")
	}

	// 显式配置：同样超限，但不降档——错误原样抛出
	srv2Handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 4096 > 2048"}}`)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(srv2Handler))
	defer srv2.Close()
	p2g, err := New(llm.Config{"api_key": "k", "model": "m", "base_url": srv2.URL, "max_tokens": "4096"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p2g.Chat(context.Background(), &llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}); err == nil {
		t.Fatal("显式配置超限应原样报错")
	}
}
