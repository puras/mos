// Package openai 提供 OpenAI 兼容协议的 Provider 适配器。
// 该协议被 DeepSeek、通义千问、GLM、Ollama 等主流服务广泛支持。
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/puras/mos/llm"
)

const defaultBaseURL = "https://api.openai.com/v1"

// Provider 是 OpenAI 兼容协议的实现。
type Provider struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// New 根据配置创建 Provider，支持键：api_key / base_url / model / http_timeout。
func New(cfg llm.Config) (llm.Provider, error) {
	p := &Provider{
		apiKey:  cfg["api_key"],
		baseURL: strings.TrimRight(cfg["base_url"], "/"),
		model:   cfg["model"],
		client:  &http.Client{},
	}
	if p.model == "" {
		return nil, errors.New("openai: missing required config \"model\"")
	}
	if p.baseURL == "" {
		p.baseURL = defaultBaseURL
	}
	return p, nil
}

// Capabilities 实现 llm.Provider。
func (p *Provider) Capabilities() llm.Capability {
	return llm.CapFunctionCalling | llm.CapStreaming
}

// Chat 实现 llm.Provider。
func (p *Provider) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return p.doChat(ctx, req, false, nil)
}

// ChatStream 实现 llm.Provider。
func (p *Provider) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return p.doChat(ctx, req, true, emit)
}

func (p *Provider) doChat(ctx context.Context, req *llm.Request, stream bool, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	body, err := json.Marshal(p.buildRequest(req, stream))
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai: unexpected status %d: %s", resp.StatusCode, string(data))
	}

	if stream {
		return p.handleStream(resp.Body, emit)
	}
	return p.handleResponse(resp.Body)
}

func (p *Provider) buildRequest(req *llm.Request, stream bool) *chatRequest {
	cr := &chatRequest{
		Model:       p.model,
		Messages:    make([]chatMessage, 0, len(req.Messages)),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	}
	for _, m := range req.Messages {
		cm := chatMessage{Role: string(m.Role), Content: m.Content}
		if m.ToolCallID != "" {
			cm.ToolCallID = m.ToolCallID
		}
		for _, tc := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: chatFunction{
					Name:      tc.Name,
					Arguments: tc.Args,
				},
			})
		}
		cr.Messages = append(cr.Messages, cm)
	}
	for _, t := range req.Tools {
		cr.Tools = append(cr.Tools, chatTool{
			Type: "function",
			Function: chatToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return cr
}

func (p *Provider) handleResponse(r io.Reader) (*llm.Response, error) {
	var raw chatResponse
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	if raw.Error != nil {
		return nil, fmt.Errorf("openai: %s (%s)", raw.Error.Message, raw.Error.Code)
	}
	if len(raw.Choices) == 0 {
		return nil, errors.New("openai: empty choices in response")
	}
	return &llm.Response{
		Message: fromChatMessage(raw.Choices[0].Message),
		Usage: llm.Usage{
			PromptTokens:     raw.Usage.PromptTokens,
			CompletionTokens: raw.Usage.CompletionTokens,
			TotalTokens:      raw.Usage.TotalTokens,
		},
	}, nil
}

// handleStream 逐行解析 SSE 数据，聚合内容与工具调用增量。
func (p *Provider) handleStream(r io.Reader, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	acc := newStreamAccumulator()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("openai: decode stream chunk: %w", err)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		acc.content.WriteString(choice.Delta.Content)
		sc := llm.StreamChunk{Delta: choice.Delta.Content, FinishReason: choice.FinishReason}
		if tc := acc.mergeToolCall(choice.Delta.ToolCalls); tc != nil {
			sc.ToolCall = tc
		}
		if sc.Delta != "" || sc.ToolCall != nil {
			if emit != nil {
				if err := emit(sc); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("openai: read stream: %w", err)
	}

	msg := llm.Message{Role: llm.RoleAssistant, Content: acc.content.String()}
	if calls := acc.finishToolCalls(); len(calls) > 0 {
		msg.ToolCalls = calls
	}
	return &llm.Response{Message: msg}, nil
}

// streamAccumulator 聚合流式输出中的文本与工具调用增量。
type streamAccumulator struct {
	content   strings.Builder
	toolCalls map[int]*streamToolCall
}

type streamToolCall struct {
	id    string
	name  string
	args  strings.Builder
	ready bool
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{toolCalls: make(map[int]*streamToolCall)}
}

// mergeToolCall 合并该块中的工具调用增量，返回已完整结束的调用（args 不再更新时无法判断，这里在有 finish_reason 后统一返回）。
// 增量场景：首个块携带 id，随后按 index 补充 name 与 arguments 片段。
func (a *streamAccumulator) mergeToolCall(deltas []chatToolCall) *llm.ToolCall {
	for _, d := range deltas {
		idx := d.Index
		tc := a.toolCalls[idx]
		if tc == nil {
			tc = &streamToolCall{}
			a.toolCalls[idx] = tc
		}
		if d.ID != "" {
			tc.id = d.ID
		}
		if d.Function.Name != "" {
			tc.name = d.Function.Name
		}
		if d.Function.Arguments != "" {
			tc.args.WriteString(d.Function.Arguments)
		}
	}
	return nil
}

// finishToolCalls 在流结束时产出完整的工具调用列表。
func (a *streamAccumulator) finishToolCalls() []llm.ToolCall {
	if len(a.toolCalls) == 0 {
		return nil
	}
	calls := make([]llm.ToolCall, 0, len(a.toolCalls))
	// index 按序输出
	for i := 0; i < len(a.toolCalls); i++ {
		tc := a.toolCalls[i]
		if tc == nil || tc.name == "" {
			continue
		}
		calls = append(calls, llm.ToolCall{
			ID:   tc.id,
			Name: tc.name,
			Args: tc.args.String(),
		})
	}
	return calls
}

// fromChatMessage 将 OpenAI 消息转换为统一模型。
func fromChatMessage(m chatMessage) llm.Message {
	out := llm.Message{
		Role:    llm.Role(m.Role),
		Content: m.Content,
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}
	return out
}

// ---- OpenAI 协议结构 ----

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type chatResponse struct {
	Choices []struct {
		Index        int         `json:"index"`
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

type chatStreamChunk struct {
	Choices []struct {
		Index        int       `json:"index"`
		Delta        chatDelta `json:"delta"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
}

type chatDelta struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls"`
}
