// Package anthropic 提供 Anthropic Claude 原生协议的 Provider 适配器。
package anthropic

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

	"hegq.net/mos/llm"
)

const (
	defaultBaseURL   = "https://api.anthropic.com"
	apiVersion       = "2023-06-01"
	defaultMaxTokens = 1024
)

// Provider 是 Anthropic 协议的实现。
type Provider struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// New 根据配置创建 Provider，支持键：api_key / base_url / model。
func New(cfg llm.Config) (llm.Provider, error) {
	p := &Provider{
		apiKey:  cfg["api_key"],
		baseURL: strings.TrimRight(cfg["base_url"], "/"),
		model:   cfg["model"],
		client:  &http.Client{},
	}
	if p.apiKey == "" {
		return nil, errors.New("anthropic: missing required config \"api_key\"")
	}
	if p.model == "" {
		return nil, errors.New("anthropic: missing required config \"model\"")
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
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic: unexpected status %d: %s", resp.StatusCode, string(data))
	}

	if stream {
		return p.handleStream(resp.Body, emit)
	}
	return p.handleResponse(resp.Body)
}

func (p *Provider) endpoint() string {
	if strings.HasSuffix(p.baseURL, "/v1") {
		return p.baseURL + "/messages"
	}
	return p.baseURL + "/v1/messages"
}

func (p *Provider) buildRequest(req *llm.Request, stream bool) *messagesRequest {
	mr := &messagesRequest{
		Model:     p.model,
		MaxTokens: defaultMaxTokens,
		Stream:    stream,
	}
	if req.MaxTokens != nil {
		mr.MaxTokens = *req.MaxTokens
	}
	mr.Temperature = req.Temperature

	var systemParts []string
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			systemParts = append(systemParts, m.Content)
		case llm.RoleUser:
			mr.Messages = append(mr.Messages, requestMessage{
				Role:    "user",
				Content: []contentBlock{{Type: "text", Text: m.Content}},
			})
		case llm.RoleAssistant:
			msg := requestMessage{Role: "assistant"}
			if m.Content != "" {
				msg.Content = append(msg.Content, contentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input any
				if err := json.Unmarshal([]byte(tc.Args), &input); err != nil {
					input = map[string]any{"raw": tc.Args}
				}
				msg.Content = append(msg.Content, contentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: input,
				})
			}
			if len(msg.Content) > 0 {
				mr.Messages = append(mr.Messages, msg)
			}
		case llm.RoleTool:
			// 工具结果合并进上一条 user 消息（Anthropic 要求 tool_result 属于 user 角色且与 tool_use 相邻）。
			block := contentBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}
			if n := len(mr.Messages); n > 0 && mr.Messages[n-1].Role == "user" {
				mr.Messages[n-1].Content = append(mr.Messages[n-1].Content, block)
			} else {
				mr.Messages = append(mr.Messages, requestMessage{
					Role:    "user",
					Content: []contentBlock{block},
				})
			}
		}
	}
	if len(systemParts) > 0 {
		mr.System = strings.Join(systemParts, "\n\n")
	}
	for _, t := range req.Tools {
		mr.Tools = append(mr.Tools, toolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	return mr
}

func (p *Provider) handleResponse(r io.Reader) (*llm.Response, error) {
	var raw messagesResponse
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	if raw.Error != nil {
		return nil, fmt.Errorf("anthropic: %s: %s", raw.Error.Type, raw.Error.Message)
	}
	msg := llm.Message{Role: llm.RoleAssistant}
	for _, block := range raw.Content {
		switch block.Type {
		case "text":
			msg.Content += block.Text
		case "tool_use":
			args, err := json.Marshal(block.Input)
			if err != nil {
				args = []byte("{}")
			}
			msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
				ID:   block.ID,
				Name: block.Name,
				Args: string(args),
			})
		}
	}
	return &llm.Response{
		Message: msg,
		Usage: llm.Usage{
			PromptTokens:     raw.Usage.InputTokens,
			CompletionTokens: raw.Usage.OutputTokens,
			TotalTokens:      raw.Usage.InputTokens + raw.Usage.OutputTokens,
		},
	}, nil
}

// handleStream 解析 Anthropic SSE 事件流，聚合文本与工具调用。
func (p *Provider) handleStream(r io.Reader, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	var (
		content      strings.Builder
		toolCalls    []llm.ToolCall
		curBlock     *streamToolBlock
		finishReason string
	)
	flush := func() error {
		if curBlock == nil {
			return nil
		}
		if curBlock.kind == "tool_use" {
			tc := llm.ToolCall{ID: curBlock.id, Name: curBlock.name, Args: curBlock.args.String()}
			toolCalls = append(toolCalls, tc)
			if emit != nil {
				if err := emit(llm.StreamChunk{ToolCall: &tc}); err != nil {
					return err
				}
			}
		}
		curBlock = nil
		return nil
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			return nil, fmt.Errorf("anthropic: decode stream event: %w", err)
		}
		switch ev.Type {
		case "content_block_start":
			if err := flush(); err != nil {
				return nil, err
			}
			curBlock = &streamToolBlock{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				content.WriteString(ev.Delta.Text)
				if emit != nil {
					if err := emit(llm.StreamChunk{Delta: ev.Delta.Text}); err != nil {
						return nil, err
					}
				}
			case "input_json_delta":
				if curBlock != nil {
					curBlock.args.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if err := flush(); err != nil {
				return nil, err
			}
		case "message_delta":
			finishReason = ev.Delta.StopReason
			if emit != nil && finishReason != "" {
				if err := emit(llm.StreamChunk{FinishReason: finishReason}); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("anthropic: read stream: %w", err)
	}

	return &llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   content.String(),
			ToolCalls: toolCalls,
		},
	}, nil
}

// streamToolBlock 记录流式内容块的工具调用增量。
type streamToolBlock struct {
	kind string
	id   string
	name string
	args strings.Builder
}

// ---- Anthropic 协议结构 ----

type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type requestMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

type messagesRequest struct {
	Model       string           `json:"model"`
	System      string           `json:"system,omitempty"`
	Messages    []requestMessage `json:"messages"`
	Tools       []toolDef        `json:"tools,omitempty"`
	MaxTokens   int              `json:"max_tokens"`
	Temperature *float64         `json:"temperature,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
}

type messagesResponse struct {
	Content []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Input any    `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type event struct {
	Type         string `json:"type"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
}
