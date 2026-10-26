// Package anthropic 提供 Anthropic Claude 原生协议的 Chat 适配器。
//
// wire 层（请求结构体、content block union、SSE 事件解析、用量转换）复用 MoGO 的
// github.com/puras/moproto/protocol/anthropic 与 github.com/puras/moproto/usage，
// 本包只保留统一 llm.ChatModel 抽象与协议映射逻辑。
package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	pkganthropic "github.com/puras/moproto/protocol/anthropic"
	pkgusage "github.com/puras/moproto/usage"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/transport"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	// defaultMaxTokens 是 max_tokens 的默认值。Anthropic 协议**强制要求**
	// 此参数，而它同时是输出的硬上限——1024 连一次像样的架构设计 + 工具
	// 调用参数都装不下，响应必然截断（finish_reason=max_tokens），截断的
	// 工具调用参数就是残缺 JSON。8192 是工具型 agent 的合理下限；
	// 宿主用 Config["max_tokens"] 覆盖。
	defaultMaxTokens = 8192
)

// Chat 是 Anthropic 协议（llm.ChatModel）的实现。
type Chat struct {
	apiKey    string
	baseURL   string
	model     string
	maxTokens int
	hc        *transport.Client
}

// New 根据配置创建 Chat，支持键：api_key / base_url / model / max_tokens / http_timeout。
// http_timeout 为 Go duration 字符串（如 "60s"）；为空时每次请求经 httpx seam
// 解析客户端（无硬超时，依赖调用方 ctx 取消）。
func New(cfg llm.Config) (llm.ChatModel, error) {
	apiKey := cfg["api_key"]
	if apiKey == "" {
		return nil, errors.New("anthropic: missing required config \"api_key\"")
	}
	model := cfg["model"]
	if model == "" {
		return nil, errors.New("anthropic: missing required config \"model\"")
	}
	baseURL := strings.TrimRight(cfg["base_url"], "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	maxTokens := defaultMaxTokens
	if v := cfg["max_tokens"]; v != "" {
		n, err := strconv.Atoi(fmt.Sprint(v))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("anthropic: invalid config \"max_tokens\": %v", v)
		}
		maxTokens = n
	}
	p := &Chat{apiKey: apiKey, baseURL: baseURL, model: model, maxTokens: maxTokens}
	hc, err := transport.New(transport.Config{
		Endpoint:    func(bool) string { return p.endpoint() },
		AuthHeaders: map[string]string{"x-api-key": apiKey, "anthropic-version": apiVersion},
		ParseError:  parseAnthropicError,
	}, cfg)
	if err != nil {
		return nil, err
	}
	p.hc = hc
	return p, nil
}

// Capabilities 实现 llm.ChatModel。
func (p *Chat) Capabilities() llm.Capability {
	return llm.CapFunctionCalling | llm.CapStreaming | llm.CapMultimodal
}

// Chat 实现 llm.ChatModel。
func (p *Chat) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return p.doChat(ctx, req, false, nil)
}

// ChatStream 实现 llm.ChatModel。
func (p *Chat) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	return p.doChat(ctx, req, true, emit)
}

func (p *Chat) doChat(ctx context.Context, req *llm.Request, stream bool, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	body, err := json.Marshal(p.buildRequest(req, stream))
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	resp, err := p.hc.Do(ctx, stream, body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, transport.ParseStatusError("anthropic", resp, parseAnthropicError)
	}

	if stream {
		return p.handleStream(resp.Body, emit)
	}
	return p.handleResponse(resp.Body)
}

func (p *Chat) endpoint() string {
	if strings.HasSuffix(p.baseURL, "/v1") {
		return p.baseURL + "/messages"
	}
	return p.baseURL + "/v1/messages"
}

func (p *Chat) buildRequest(req *llm.Request, stream bool) *pkganthropic.AnthropicRequest {
	ar := &pkganthropic.AnthropicRequest{
		Model:       p.model,
		MaxTokens:   p.maxTokens,
		Temperature: req.Temperature,
		Stream:      stream,
	}
	if req.MaxTokens != nil {
		ar.MaxTokens = *req.MaxTokens
	}
	// ReasoningEffort → extended thinking（enabled + budget_tokens）。
	// budget 约束 ≥1024 且 < max_tokens，上限不足时提升。
	if effort := req.ReasoningEffort; effort != nil {
		budget := thinkingBudget(*effort)
		ar.Thinking = pkganthropic.AnthropicThinkingConfigEnabled{
			Type:         pkganthropic.AnthropicThinkingConfigEnabledTypeEnabled,
			BudgetTokens: budget,
		}
		if int64(ar.MaxTokens) <= budget {
			ar.MaxTokens = int(budget) + defaultMaxTokens
		}
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

// thinkingBudget 将统一 ReasoningEffort（low|medium|high）映射为 Anthropic
// extended thinking 的 budget_tokens 档位。
func thinkingBudget(effort string) int64 {
	switch effort {
	case "low":
		return 1024
	case "high":
		return 16384
	default: // medium 及未识别值
		return 4096
	}
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
// moproto 的 AnthropicMessageResponse 可直接解码（content 为带自定义 Unmarshal 的
// AnthropicContentBlockUnion 切片），这里保留最小镜像仅用于统一错误结构。

// anthropicError 是响应错误结构的解码镜像（9 种错误变体均为 Type + Message）。
type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// parseAnthropicError 解析 Anthropic 错误响应体（非 200 路径，供 transport.ParseStatusError 使用）。
func parseAnthropicError(statusCode int, body []byte) (string, bool) {
	var errResp struct {
		Error *anthropicError `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != nil {
		return fmt.Sprintf("%s: %s", errResp.Error.Type, errResp.Error.Message), true
	}
	return "", false
}

type messagesResponse struct {
	Content []pkganthropic.AnthropicContentBlockUnion `json:"content"`
	Usage   *pkganthropic.AnthropicUsage              `json:"usage"`
	Stop    *string                                   `json:"stop_reason"`
	Error   *anthropicError                           `json:"error"`
}

func (p *Chat) handleResponse(r io.Reader) (*llm.Response, error) {
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
		case string(pkganthropic.AnthropicThinkingBlockTypeThinking):
			var b pkganthropic.AnthropicThinkingBlock
			if err := json.Unmarshal(block.Thinking, &b); err == nil {
				msg.ReasoningContent += b.Thinking
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
		Message:      msg,
		Usage:        transport.UsageFrom(pkgusage.FromAnthropic(raw.Usage)),
		FinishReason: anthropicStopReason(raw.Stop),
	}, nil
}

// handleStream 解析 Anthropic SSE 事件流，聚合文本与工具调用。
func (p *Chat) handleStream(r io.Reader, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	var (
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    []llm.ToolCall
		curBlock     *streamToolBlock
		finishReason string
		usage        *pkgusage.Usage
	)
	flush := func() error {
		if curBlock == nil {
			return nil
		}
		switch curBlock.kind {
		case "tool_use":
			tc := llm.ToolCall{ID: curBlock.id, Name: curBlock.name, Args: curBlock.args.String()}
			toolCalls = append(toolCalls, tc)
			if emit != nil {
				if err := emit(llm.StreamChunk{ToolCall: &tc}); err != nil {
					return err
				}
			}
		case "thinking":
			reasoning.WriteString(curBlock.text.String())
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
			case string(pkganthropic.AnthropicThinkingDeltaBlockTypeThinkingDelta):
				var b pkganthropic.AnthropicThinkingDeltaBlock
				if err := json.Unmarshal(e.Delta.ThinkingDelta, &b); err == nil {
					if curBlock != nil && curBlock.kind == "thinking" {
						curBlock.text.WriteString(b.Thinking)
					}
					if emit != nil {
						if err := emit(llm.StreamChunk{Reasoning: b.Thinking}); err != nil {
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
	// 流完整性：正常结束必带 message_delta.stop_reason。没收到就结束 =
	// 流被掐断（连接断开/服务端异常）——半截文本 + 残缺 tool_use（参数
	// JSON 只传了一半）不能当正常响应返回。
	if finishReason == "" {
		return nil, fmt.Errorf("anthropic: %w", llm.ErrStreamTruncated)
	}

	msg := llm.Message{Role: llm.RoleAssistant, Content: content.String()}
	if reasoning.Len() > 0 {
		msg.ReasoningContent = reasoning.String()
	}
	msg.ToolCalls = toolCalls
	resp := &llm.Response{Message: msg}
	resp.FinishReason = finishReason
	if usage != nil {
		resp.Usage = transport.UsageFrom(usage)
	}
	return resp, nil
}

// anthropicStopReason 将 Anthropic stop_reason 映射为统一停止原因。
// 协议原文为 end_turn / max_tokens / stop_sequence / tool_use，原样透传即可。
func anthropicStopReason(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// streamToolBlock 记录流式内容块的增量（tool_use 的参数 JSON、thinking 的文本）。
type streamToolBlock struct {
	kind string
	id   string
	name string
	args strings.Builder
	text strings.Builder // thinking 块文本增量
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
