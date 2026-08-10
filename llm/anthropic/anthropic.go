// Package anthropic 提供 Anthropic Claude 原生协议的 Model 适配器。
//
// wire 层（请求结构体、content block union、SSE 事件解析、用量转换）复用 MoGO 的
// hegq.net/mogo/pkg/protocol/anthropic 与 hegq.net/mogo/pkg/usage，
// 本包只保留统一 llm.ChatModel 抽象与协议映射逻辑。
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

	pkganthropic "hegq.net/mogo/pkg/protocol/anthropic"
	pkgusage "hegq.net/mogo/pkg/usage"

	"hegq.net/mos/llm"
)

const (
	defaultBaseURL   = "https://api.anthropic.com"
	apiVersion       = "2023-06-01"
	defaultMaxTokens = 1024
)

// Model 是 Anthropic 协议的实现。
type Model struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// New 根据配置创建 Model，支持键：api_key / base_url / model。
func New(cfg llm.Config) (llm.ChatModel, error) {
	p := &Model{
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

// Capabilities 实现 llm.ChatModel。
func (p *Model) Capabilities() llm.Capability {
	return llm.CapFunctionCalling | llm.CapStreaming | llm.CapMultimodal
}

// Chat 实现 llm.ChatModel。
func (p *Model) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return p.doChat(ctx, req, false, nil)
}

// ChatStream 实现 llm.ChatModel。
func (p *Model) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return p.doChat(ctx, req, true, emit)
}

func (p *Model) doChat(ctx context.Context, req *llm.Request, stream bool, emit func(llm.StreamChunk) error) (*llm.Response, error) {
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
		var errResp struct {
			Type  string `json:"type"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &errResp) == nil && errResp.Error != nil {
			return nil, fmt.Errorf("anthropic: %s: %s", errResp.Error.Type, errResp.Error.Message)
		}
		return nil, fmt.Errorf("anthropic: unexpected status %d: %s", resp.StatusCode, string(data))
	}

	if stream {
		return p.handleStream(resp.Body, emit)
	}
	return p.handleResponse(resp.Body)
}

func (p *Model) endpoint() string {
	if strings.HasSuffix(p.baseURL, "/v1") {
		return p.baseURL + "/messages"
	}
	return p.baseURL + "/v1/messages"
}

func (p *Model) buildRequest(req *llm.Request, stream bool) *pkganthropic.AnthropicRequest {
	ar := &pkganthropic.AnthropicRequest{
		Model:       p.model,
		MaxTokens:   defaultMaxTokens,
		Temperature: req.Temperature,
		Stream:      stream,
	}
	if req.MaxTokens != nil {
		ar.MaxTokens = *req.MaxTokens
	}

	var systemParts []string
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			systemParts = append(systemParts, m.Content)
		case llm.RoleUser:
			ar.Messages = append(ar.Messages, pkganthropic.AnthropicMessageParam{
				Role:    pkganthropic.AnthropicMessageRoleUser,
				Content: anthropicUserContent(m),
			})
		case llm.RoleAssistant:
			msg := pkganthropic.AnthropicMessageParam{
				Role:    pkganthropic.AnthropicMessageRoleAssistant,
				Content: anthropicAssistantContent(m),
			}
			if len(msg.Content) > 0 {
				ar.Messages = append(ar.Messages, msg)
			}
		case llm.RoleTool:
			// 工具结果合并进上一条 user 消息（Anthropic 要求 tool_result 属于 user 角色且与 tool_use 相邻）。
			block, _ := json.Marshal(pkganthropic.AnthropicToolResultBlockParam{
				Type:      pkganthropic.AnthropicToolResultBlockTypeToolResult,
				ToolUseID: m.ToolCallID,
				Content:   rawString(m.Content),
			})
			if n := len(ar.Messages); n > 0 && ar.Messages[n-1].Role == pkganthropic.AnthropicMessageRoleUser {
				var blocks []json.RawMessage
				if err := json.Unmarshal(ar.Messages[n-1].Content, &blocks); err == nil {
					blocks = append(blocks, block)
					if data, err := json.Marshal(blocks); err == nil {
						ar.Messages[n-1].Content = data
						continue
					}
				}
			}
			ar.Messages = append(ar.Messages, pkganthropic.AnthropicMessageParam{
				Role:    pkganthropic.AnthropicMessageRoleUser,
				Content: rawBlocks(block),
			})
		}
	}
	if len(systemParts) > 0 {
		ar.System = rawString(strings.Join(systemParts, "\n\n"))
	}
	for _, t := range req.Tools {
		ar.Tools = append(ar.Tools, pkganthropic.NewAnthropicCustomTool(
			t.Name,
			toAnthropicInputSchema(t.Parameters),
			pkganthropic.WithDescription(t.Description),
		))
	}
	return ar
}

// anthropicUserContent 将统一 user 消息展开为 Anthropic content 数组。
func anthropicUserContent(m llm.Message) json.RawMessage {
	if len(m.ContentParts) > 0 {
		data, _ := json.Marshal(anthropicContent(m.ContentParts))
		return data
	}
	data, _ := json.Marshal([]pkganthropic.AnthropicContentBlockParam{
		pkganthropic.AnthropicTextBlockParam{Type: pkganthropic.AnthropicTextBlockTypeText, Text: m.Content},
	})
	return data
}

// anthropicAssistantContent 将统一 assistant 消息展开为 Anthropic content 数组
// （text 块 + tool_use 块），无内容时返回空。
func anthropicAssistantContent(m llm.Message) json.RawMessage {
	var blocks []pkganthropic.AnthropicContentBlockParam
	if m.Content != "" {
		blocks = append(blocks, pkganthropic.AnthropicTextBlockParam{
			Type: pkganthropic.AnthropicTextBlockTypeText,
			Text: m.Content,
		})
	}
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, pkganthropic.AnthropicToolUseBlockParam{
			Type:  pkganthropic.AnthropicToolUseBlockTypeToolUse,
			ID:    tc.ID,
			Name:  tc.Name,
			Input: json.RawMessage(tc.Args),
		})
	}
	if len(blocks) == 0 {
		return nil
	}
	data, _ := json.Marshal(blocks)
	return data
}

// anthropicContent 将统一的内容块展开为 Anthropic content block 数组。
// text → text 块；image → image 块（base64 或 URL）；file → document 块；audio 暂不支持被忽略。
func anthropicContent(parts []llm.ContentPart) []pkganthropic.AnthropicContentBlockParam {
	out := make([]pkganthropic.AnthropicContentBlockParam, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "image":
			if p.URL != "" {
				out = append(out, pkganthropic.AnthropicImageBlockParam{
					Type:   pkganthropic.AnthropicImageBlockTypeImage,
					Source: pkganthropic.NewAnthropicImageSourceURL(p.URL),
				})
			} else {
				mime := p.MIME
				if mime == "" {
					mime = "image/png"
				}
				out = append(out, pkganthropic.AnthropicImageBlockParam{
					Type:   pkganthropic.AnthropicImageBlockTypeImage,
					Source: pkganthropic.NewAnthropicImageSourceBase64(mime, p.Data),
				})
			}
		case "file":
			out = append(out, pkganthropic.AnthropicDocumentBlockParam{
				Type:   pkganthropic.AnthropicDocumentBlockTypeDocument,
				Source: pkganthropic.NewAnthropicDocumentSourceBase64(p.MIME, p.Data),
			})
		default: // text
			out = append(out, pkganthropic.AnthropicTextBlockParam{
				Type: pkganthropic.AnthropicTextBlockTypeText,
				Text: p.Text,
			})
		}
	}
	return out
}

// toAnthropicInputSchema 将统一工具的 JSON Schema 参数转换为 Anthropic input_schema。
func toAnthropicInputSchema(params any) pkganthropic.AnthropicToolInputSchema {
	schema := pkganthropic.AnthropicToolInputSchema{Type: "object"}
	if params == nil {
		return schema
	}
	data, err := json.Marshal(params)
	if err != nil {
		return schema
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return schema
	}
	for k, v := range m {
		switch k {
		case "type":
			if s, ok := v.(string); ok && s != "" {
				schema.Type = s
			}
		case "properties":
			schema.Properties = v
		case "required":
			if list, ok := v.([]any); ok {
				for _, item := range list {
					if s, ok := item.(string); ok {
						schema.Required = append(schema.Required, s)
					}
				}
			}
		default:
			if schema.ExtraFields == nil {
				schema.ExtraFields = map[string]any{}
			}
			schema.ExtraFields[k] = v
		}
	}
	return schema
}

// ---- 响应解析 ----
//
// mogo 的 AnthropicMessageResponse 可直接解码（content 为带自定义 Unmarshal 的
// AnthropicContentBlockUnion 切片），这里保留最小镜像仅用于统一错误结构。

// anthropicError 是响应错误结构的解码镜像（9 种错误变体均为 Type + Message）。
type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type messagesResponse struct {
	Content []pkganthropic.AnthropicContentBlockUnion `json:"content"`
	Usage   *pkganthropic.AnthropicUsage              `json:"usage"`
	Error   *anthropicError                           `json:"error"`
}

func (p *Model) handleResponse(r io.Reader) (*llm.Response, error) {
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
		case string(pkganthropic.AnthropicTextBlockTypeText):
			var b pkganthropic.AnthropicTextBlock
			if err := json.Unmarshal(block.Text, &b); err == nil {
				msg.Content += b.Text
			}
		case string(pkganthropic.AnthropicToolUseBlockTypeToolUse):
			var b pkganthropic.AnthropicToolUseBlock
			if err := json.Unmarshal(block.ToolUse, &b); err == nil {
				msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
					ID:   b.ID,
					Name: b.Name,
					Args: string(b.Input),
				})
			}
		}
	}
	return &llm.Response{
		Message: msg,
		Usage:   toLLMUsage(pkgusage.FromAnthropic(raw.Usage)),
	}, nil
}

// toLLMUsage 将 MoGO 统一用量映射为 MoS 统一结构，含缓存/推理明细。
func toLLMUsage(u *pkgusage.Usage) llm.Usage {
	out := llm.Usage{}
	if u == nil {
		return out
	}
	out.PromptTokens = int(u.InputTokens)
	out.CompletionTokens = int(u.OutputTokens)
	out.TotalTokens = int(u.TotalTokens)
	if u.Cache != nil {
		out.Details.Input.CacheRead = int(u.Cache.ReadInputTokens)
		out.Details.Input.CacheWrite = int(u.Cache.CreationInputTokens)
	}
	if u.Thinking != nil {
		out.Details.Output.Reasoning = int(u.Thinking.ThinkingTokens)
	}
	return out
}

// handleStream 解析 Anthropic SSE 事件流，聚合文本与工具调用。
func (p *Model) handleStream(r io.Reader, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	var (
		content      strings.Builder
		toolCalls    []llm.ToolCall
		curBlock     *streamToolBlock
		finishReason string
		usage        *pkgusage.Usage
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
		evt, err := pkganthropic.ParseSSEEvent(line)
		if err != nil {
			return nil, fmt.Errorf("anthropic: parse stream event: %w", err)
		}
		if evt == nil {
			continue
		}
		switch e := evt.(type) {
		case pkganthropic.AnthropicContentBlockStartEvent:
			if err := flush(); err != nil {
				return nil, err
			}
			if e.ContentBlock != nil {
				if e.ContentBlock.Type == string(pkganthropic.AnthropicToolUseBlockTypeToolUse) {
					var b pkganthropic.AnthropicToolUseBlock
					if err := json.Unmarshal(e.ContentBlock.ToolUse, &b); err == nil {
						curBlock = &streamToolBlock{kind: "tool_use", id: b.ID, name: b.Name}
					}
				} else {
					curBlock = &streamToolBlock{kind: e.ContentBlock.Type}
				}
			}
		case pkganthropic.AnthropicContentBlockDeltaEvent:
			if e.Delta == nil {
				continue
			}
			switch e.Delta.Type {
			case string(pkganthropic.AnthropicTextDeltaBlockTypeTextDelta):
				var b pkganthropic.AnthropicTextDeltaBlock
				if err := json.Unmarshal(e.Delta.TextDelta, &b); err == nil {
					content.WriteString(b.Text)
					if emit != nil {
						if err := emit(llm.StreamChunk{Delta: b.Text}); err != nil {
							return nil, err
						}
					}
				}
			case string(pkganthropic.AnthropicInputJSONDeltaBlockTypeInputJSONDelta):
				if curBlock != nil {
					var b pkganthropic.AnthropicInputJSONDeltaBlock
					if err := json.Unmarshal(e.Delta.InputJSONDelta, &b); err == nil {
						curBlock.args.WriteString(b.PartialJSON)
					}
				}
			}
		case pkganthropic.AnthropicContentBlockStopEvent:
			if err := flush(); err != nil {
				return nil, err
			}
		case pkganthropic.AnthropicMessageDeltaEvent:
			if e.Delta != nil && e.Delta.StopReason != nil {
				finishReason = string(*e.Delta.StopReason)
				if emit != nil && finishReason != "" {
					if err := emit(llm.StreamChunk{FinishReason: finishReason}); err != nil {
						return nil, err
					}
				}
			}
			// message_delta 的 usage 是累计快照（最后一条即最终用量），last-wins。
			if e.Usage != nil {
				usage = pkgusage.FromAnthropicMessageDelta(e.Usage)
			}
		case pkganthropic.AnthropicErrorStreamEvent:
			if e.Error != nil && e.Error.Error != nil {
				return nil, fmt.Errorf("anthropic: stream error: %s", e.Error.Error.Type)
			}
			return nil, errors.New("anthropic: stream error")
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("anthropic: read stream: %w", err)
	}

	resp := &llm.Response{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   content.String(),
			ToolCalls: toolCalls,
		},
	}
	if usage != nil {
		resp.Usage = toLLMUsage(usage)
	}
	return resp, nil
}

// streamToolBlock 记录流式内容块的工具调用增量。
type streamToolBlock struct {
	kind string
	id   string
	name string
	args strings.Builder
}

// rawString 将字符串编码为 JSON 原始值（用于 Content / System 等 string 形态字段）。
func rawString(s string) json.RawMessage {
	data, _ := json.Marshal(s)
	return data
}

// rawBlocks 将单个 block 包裹为 JSON 数组（用于 tool_result 等数组形态 Content）。
func rawBlocks(blocks ...json.RawMessage) json.RawMessage {
	data, _ := json.Marshal(blocks)
	return data
}
