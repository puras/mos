// Package openai 提供 OpenAI 兼容协议的 Chat 适配器。
// 该协议被 DeepSeek、通义千问、GLM、Ollama 等主流服务广泛支持。
//
// wire 层（请求结构体、union、SSE 解析、用量转换）复用 MoGO 的
// github.com/puras/moproto/protocol/openai 与 github.com/puras/moproto/usage，
// 本包只保留统一 llm.ChatModel 抽象与协议映射逻辑。
package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	pkgopenai "github.com/puras/moproto/protocol/openai"
	pkgusage "github.com/puras/moproto/usage"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/transport"
)

const defaultBaseURL = "https://api.openai.com/v1"

// Chat 是 OpenAI 兼容协议（llm.ChatModel）的实现。
type Chat struct {
	apiKey  string
	baseURL string
	model   string
	hc      *transport.Client
}

// New 根据配置创建 Chat，支持键：api_key / base_url / model / http_timeout。
// http_timeout 为 Go duration 字符串（如 "60s"、"2m"），设置单次请求超时；
// 为空时每次请求经 httpx seam 解析客户端（无硬超时，依赖调用方 ctx 取消）。
// api_key 可空（本地 Ollama 等无鉴权端点）。
func New(cfg llm.Config) (llm.ChatModel, error) {
	model := cfg["model"]
	if model == "" {
		return nil, errors.New("openai: missing required config \"model\"")
	}
	baseURL := strings.TrimRight(cfg["base_url"], "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	p := &Chat{
		apiKey:  cfg["api_key"],
		baseURL: baseURL,
		model:   model,
	}
	hc, err := transport.New(transport.Config{
		Endpoint:    func(bool) string { return p.baseURL + "/chat/completions" },
		AuthHeaders: map[string]string{"Authorization": "Bearer " + p.apiKey},
		ParseError:  parseOpenAIError,
	}, cfg)
	if err != nil {
		return nil, err
	}
	p.hc = hc
	return p, nil
}

// Capabilities 实现 llm.ChatModel。
func (p *Chat) Capabilities() llm.Capability {
	return llm.CapFunctionCalling | llm.CapStreaming |
		llm.CapStructuredOutput | llm.CapMultimodal | llm.CapReasoning
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
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}
	body = injectReasoningContent(body, req.Messages)
	resp, err := p.hc.Do(ctx, stream, body)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, transport.ParseStatusError("openai", resp, parseOpenAIError)
	}

	if stream {
		return p.handleStream(resp.Body, emit)
	}
	return p.handleResponse(resp.Body)
}

// parseOpenAIError 解析 OpenAI 错误响应体（非 200 路径，供 transport.ParseStatusError 使用）。
func parseOpenAIError(statusCode int, body []byte) (string, bool) {
	var errResp pkgopenai.OpenAIErrorResponse
	if json.Unmarshal(body, &errResp) == nil && errResp.Error.Message != "" {
		code := ""
		if errResp.Error.Code != nil {
			code = *errResp.Error.Code
		}
		return fmt.Sprintf("%s (%s)", errResp.Error.Message, code), true
	}
	return "", false
}

func (p *Chat) buildRequest(req *llm.Request, stream bool) *pkgopenai.OpenAIChatCompletionParams {
	cr := &pkgopenai.OpenAIChatCompletionParams{
		Model:       p.model,
		Temperature: req.Temperature,
		Stream:      boolPtr(stream),
	}
	if req.MaxTokens != nil {
		v := int64(*req.MaxTokens)
		cr.MaxTokens = &v
	}
	if stream {
		// 请求流式场景附带 token 用量（OpenAI stream_options.include_usage）。
		cr.StreamOptions = &pkgopenai.OpenAIChatCompletionStreamOptionsParam{IncludeUsage: boolPtr(true)}
	}
	for _, m := range req.Modalities {
		cr.Modalities = append(cr.Modalities, pkgopenai.OpenAIModality(m))
	}
	if req.ReasoningEffort != nil {
		cr.ReasoningEffort = pkgopenai.OpenAIReasoningEffort(*req.ReasoningEffort)
	}
	if req.ResponseFormat != nil {
		cr.ResponseFormat = toOpenAIResponseFormat(req.ResponseFormat)
	}
	for _, m := range req.Messages {
		cr.Messages = append(cr.Messages, toOpenAIMessage(m))
	}
	for _, t := range req.Tools {
		params, err := json.Marshal(t.Parameters)
		if err != nil {
			params = nil
		}
		cr.Tools = append(cr.Tools, &pkgopenai.OpenAIChatCompletionToolUnionFlat{
			Type: "function",
			Function: &pkgopenai.OpenAIChatCompletionToolFunctionParam{
				Type: "function",
				Function: pkgopenai.OpenAIChatCompletionToolFunctionDefinition{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			},
		})
	}
	return cr
}

// toOpenAIResponseFormat 将统一的结构化输出格式映射为 OpenAI response_format union。
func toOpenAIResponseFormat(rf *llm.ResponseFormat) pkgopenai.OpenAIResponseFormatUnion {
	flat := &pkgopenai.OpenAIResponseFormatUnionFlat{}
	switch rf.Type {
	case "json_object":
		flat.Type = "json_object"
		flat.JSONObject = &pkgopenai.OpenAIResponseFormatJSONObjectParam{Type: pkgopenai.OpenAIResponseFormatTypeJSONObject}
	case "json_schema":
		schema, _ := json.Marshal(rf.Schema)
		flat.Type = "json_schema"
		flat.JSONSchema = &pkgopenai.OpenAIResponseFormatJSONSchemaParam{
			Type: pkgopenai.OpenAIResponseFormatTypeJSONSchema,
			JSONSchema: pkgopenai.OpenAIJSONSchema{
				Name:   rf.Name,
				Schema: schema,
			},
		}
	default:
		flat.Type = "text"
		flat.Text = &pkgopenai.OpenAIResponseFormatTextParam{Type: pkgopenai.OpenAIResponseFormatTypeText}
	}
	return flat
}

// toOpenAIMessage 将统一消息映射为 OpenAI 协议消息 union。
func toOpenAIMessage(m llm.Message) pkgopenai.OpenAIChatCompletionMessageParamUnion {
	switch m.Role {
	case llm.RoleSystem:
		return &pkgopenai.OpenAIChatCompletionSystemMessageParam{
			Role:    pkgopenai.OpenAIChatCompletionMessageRoleSystem,
			Content: toOpenAISystemContent(m),
		}
	case llm.RoleAssistant:
		am := &pkgopenai.OpenAIChatCompletionAssistantMessageParam{
			Role: pkgopenai.OpenAIChatCompletionMessageRoleAssistant,
		}
		if m.Content != "" {
			c := m.Content
			am.Content = &c
		}
		for _, tc := range m.ToolCalls {
			am.ToolCalls = append(am.ToolCalls, &pkgopenai.OpenAIChatCompletionMessageToolCallUnionFlat{
				Type: "function",
				Function: &pkgopenai.OpenAIChatCompletionMessageToolCallFunctionParam{
					ID:   tc.ID,
					Type: "function",
					Function: pkgopenai.OpenAIChatCompletionMessageToolCallFunction{
						Name:      tc.Name,
						Arguments: tc.Args,
					},
				},
			})
		}
		return am
	case llm.RoleTool:
		s := pkgopenai.OpenAIChatCompletionToolMessageContentString(m.Content)
		return &pkgopenai.OpenAIChatCompletionToolMessageParam{
			Role:       pkgopenai.OpenAIChatCompletionMessageRoleTool,
			Content:    &pkgopenai.OpenAIChatCompletionToolMessageContentUnionFlat{Type: "string", String: &s},
			ToolCallID: m.ToolCallID,
		}
	default: // user
		um := &pkgopenai.OpenAIChatCompletionUserMessageParam{
			Role: pkgopenai.OpenAIChatCompletionMessageRoleUser,
		}
		if len(m.ContentParts) > 0 {
			parts := pkgopenai.OpenAIChatCompletionUserMessageContentParts(toOpenAIContent(m.ContentParts))
			um.Content = &pkgopenai.OpenAIChatCompletionUserMessageContentUnionFlat{Type: "parts", Parts: &parts}
		} else {
			s := pkgopenai.OpenAIChatCompletionUserMessageContentString(m.Content)
			um.Content = &pkgopenai.OpenAIChatCompletionUserMessageContentUnionFlat{Type: "string", String: &s}
		}
		return um
	}
}

// injectReasoningContent 在请求体中为 assistant 消息注入 reasoning_content（DeepSeek/Qwen 扩展）。
// moproto 请求侧 assistant 消息暂无该字段（响应侧已支持），此处按消息顺序做最小字节级注入，
// 只重编码 messages 数组，其余字段保留原始字节；待 moproto 支持后可移除。
func injectReasoningContent(body []byte, messages []llm.Message) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	rawMsgs, ok := top["messages"]
	if !ok {
		return body
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return body
	}
	changed := false
	for i, m := range messages {
		if m.Role != llm.RoleAssistant || m.ReasoningContent == "" || i >= len(msgs) {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(msgs[i], &obj); err != nil {
			continue
		}
		rc, _ := json.Marshal(m.ReasoningContent)
		obj["reasoning_content"] = rc
		updated, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		msgs[i] = updated
		changed = true
	}
	if !changed {
		return body
	}
	updatedMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body
	}
	top["messages"] = updatedMsgs
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

func toOpenAISystemContent(m llm.Message) pkgopenai.OpenAIChatCompletionSystemMessageContentUnion {
	if len(m.ContentParts) > 0 {
		parts := pkgopenai.OpenAIChatCompletionSystemMessageContentParts(toOpenAIContent(m.ContentParts))
		return &pkgopenai.OpenAIChatCompletionSystemMessageContentUnionFlat{Type: "parts", Parts: &parts}
	}
	s := pkgopenai.OpenAIChatCompletionSystemMessageContentString(m.Content)
	return &pkgopenai.OpenAIChatCompletionSystemMessageContentUnionFlat{Type: "string", String: &s}
}

// toOpenAIContent 将统一的内容块展开为 OpenAI content 数组格式。
// image 与 file 走 image_url（支持 data URL），audio 走 input_audio。
func toOpenAIContent(parts []llm.ContentPart) []pkgopenai.OpenAIChatCompletionContentPartUnion {
	out := make([]pkgopenai.OpenAIChatCompletionContentPartUnion, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "image":
			out = append(out, &pkgopenai.OpenAIChatCompletionContentPartUnionFlat{
				Type: "image_url",
				Image: &pkgopenai.OpenAIChatCompletionContentPartImageParam{
					Type:     pkgopenai.OpenAIChatCompletionContentPartTypeImageURL,
					ImageURL: pkgopenai.OpenAIChatCompletionContentPartImageURL{URL: mediaURL(p)},
				},
			})
		case "audio":
			out = append(out, &pkgopenai.OpenAIChatCompletionContentPartUnionFlat{
				Type: "input_audio",
				InputAudio: &pkgopenai.OpenAIChatCompletionContentPartInputAudioParam{
					Type: pkgopenai.OpenAIChatCompletionContentPartTypeInputAudio,
					InputAudio: pkgopenai.OpenAIChatCompletionContentPartInputAudio{
						Data:   p.Data,
						Format: pkgopenai.OpenAIAudioFormat(audioFormat(p.MIME)),
					},
				},
			})
		case "file":
			out = append(out, &pkgopenai.OpenAIChatCompletionContentPartUnionFlat{
				Type: "image_url",
				Image: &pkgopenai.OpenAIChatCompletionContentPartImageParam{
					Type:     pkgopenai.OpenAIChatCompletionContentPartTypeImageURL,
					ImageURL: pkgopenai.OpenAIChatCompletionContentPartImageURL{URL: mediaURL(p)},
				},
			})
		default: // text
			out = append(out, &pkgopenai.OpenAIChatCompletionContentPartUnionFlat{
				Type: "text",
				Text: &pkgopenai.OpenAIChatCompletionContentPartTextParam{
					Type: pkgopenai.OpenAIChatCompletionContentPartTypeText,
					Text: p.Text,
				},
			})
		}
	}
	return out
}

// mediaURL 优先使用远程 URL，否则将 base64 数据包装为 data URL。
func mediaURL(p llm.ContentPart) string {
	if p.URL != "" {
		return p.URL
	}
	mime := p.MIME
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + p.Data
}

// audioFormat 从 MIME 类型推断 OpenAI input_audio 的 format 参数。
func audioFormat(mime string) string {
	switch mime {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/wav", "audio/x-wav":
		return "wav"
	case "audio/ogg":
		return "ogg"
	case "audio/aac":
		return "aac"
	default:
		return "mp3"
	}
}

// ---- 响应解析 ----
//
// moproto 的响应结构（OpenAIChatCompletion 等）是 marshal 导向的（工具调用为 union 接口切片，
// encoding/json 无法直接反序列化），因此这里保留一个最小的解码镜像结构，
// 其中 usage / 错误 / 工具调用元素仍使用 moproto 类型解码。

// chatChoiceMessage 是响应 message 的解码镜像，tool_calls 以原始字节暂存后逐条用 moproto union 解析。
type chatChoiceMessage struct {
	Role             string            `json:"role"`
	Content          string            `json:"content"`
	ToolCalls        []json.RawMessage `json:"tool_calls"`
	Reasoning        string            `json:"reasoning"`
	ReasoningContent string            `json:"reasoning_content"`
}

type chatResponse struct {
	Choices []struct {
		Index        int64             `json:"index"`
		Message      chatChoiceMessage `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage *pkgopenai.OpenAICompletionUsage `json:"usage"`
	Error *pkgopenai.OpenAIError           `json:"error"`
}

func (p *Chat) handleResponse(r io.Reader) (*llm.Response, error) {
	var raw chatResponse
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	if raw.Error != nil {
		code := ""
		if raw.Error.Code != nil {
			code = *raw.Error.Code
		}
		return nil, fmt.Errorf("openai: %s (%s)", raw.Error.Message, code)
	}
	if len(raw.Choices) == 0 {
		return nil, errors.New("openai: empty choices in response")
	}
	return &llm.Response{
		Message: fromChatMessage(raw.Choices[0].Message),
		Usage:   transport.UsageFrom(pkgusage.FromOpenAICompletion(raw.Usage)),
	}, nil
}

// handleStream 逐行解析 SSE 数据，聚合内容与工具调用增量。
func (p *Chat) handleStream(r io.Reader, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	acc := newStreamAccumulator()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		evt, err := pkgopenai.ParseSSEEvent(line)
		if err != nil {
			return nil, fmt.Errorf("openai: parse stream event: %w", err)
		}
		if evt == nil || len(evt.Chunk) == 0 {
			// 非 chunk 事件（含 [DONE]）结束
			if evt == nil {
				continue
			}
			break
		}
		var chunk pkgopenai.OpenAIChatCompletionStreamChunkEvent
		if err := json.Unmarshal(evt.Chunk, &chunk); err != nil {
			return nil, fmt.Errorf("openai: decode stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			acc.usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if fr := deref(choice.FinishReason); fr != "" {
			acc.finish = fr
		}
		acc.content.WriteString(choice.Delta.Content)
		acc.reasoning.WriteString(choice.Delta.ReasoningContent)
		sc := llm.StreamChunk{Delta: choice.Delta.Content, Reasoning: choice.Delta.ReasoningContent, FinishReason: deref(choice.FinishReason)}
		if tc := acc.mergeToolCall(choice.Delta.ToolCalls); tc != nil {
			sc.ToolCall = tc
		}
		if sc.Delta != "" || sc.Reasoning != "" || sc.ToolCall != nil {
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
	if acc.reasoning.Len() > 0 {
		msg.ReasoningContent = acc.reasoning.String()
	}
	if calls := acc.finishToolCalls(); len(calls) > 0 {
		msg.ToolCalls = calls
	}
	resp := &llm.Response{Message: msg}
	resp.FinishReason = acc.finish
	if acc.usage != nil {
		resp.Usage = transport.UsageFrom(pkgusage.FromOpenAICompletion(acc.usage))
	}
	return resp, nil
}

// streamAccumulator 聚合流式输出中的文本与工具调用增量。
type streamAccumulator struct {
	content   strings.Builder
	reasoning strings.Builder // reasoning_content 增量（DeepSeek/Qwen 扩展）
	toolCalls map[int]*streamToolCall
	usage     *pkgopenai.OpenAICompletionUsage
	finish    string // 最后一次 finish_reason（last-wins，通常仅终止块携带）
}

type streamToolCall struct {
	id   string
	name string
	args strings.Builder
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{toolCalls: make(map[int]*streamToolCall)}
}

// mergeToolCall 合并该块中的工具调用增量，返回已完整结束的调用（args 不再更新时无法判断，这里在流结束统一返回）。
func (a *streamAccumulator) mergeToolCall(deltas []pkgopenai.OpenAIChatCompletionStreamChunkChoiceDeltaToolCall) *llm.ToolCall {
	for _, d := range deltas {
		idx := int(d.Index)
		tc := a.toolCalls[idx]
		if tc == nil {
			tc = &streamToolCall{}
			a.toolCalls[idx] = tc
		}
		if d.ID != "" {
			tc.id = d.ID
		}
		if d.Function != nil {
			if d.Function.Name != "" {
				tc.name = d.Function.Name
			}
			if d.Function.Arguments != "" {
				tc.args.WriteString(d.Function.Arguments)
			}
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
func fromChatMessage(m chatChoiceMessage) llm.Message {
	out := llm.Message{Role: llm.Role(m.Role)}
	if m.Content != "" {
		out.Content = m.Content
	}
	// reasoning_content（DeepSeek/Qwen 扩展）优先，reasoning（部分厂商等价字段）兜底。
	if m.ReasoningContent != "" {
		out.ReasoningContent = m.ReasoningContent
	} else {
		out.ReasoningContent = m.Reasoning
	}
	for _, raw := range m.ToolCalls {
		var tc pkgopenai.OpenAIChatCompletionChoiceMessageToolCallUnionFlat
		if err := json.Unmarshal(raw, &tc); err != nil {
			continue
		}
		if tc.Function != nil {
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
				ID:   tc.Function.ID,
				Name: tc.Function.Function.Name,
				Args: tc.Function.Function.Arguments,
			})
		}
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
