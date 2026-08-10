// Package gemini 提供 Google Gemini Generative Language API 的 Model 适配器。
//
// 覆盖 ChatCompletion（generateContent + streamGenerateContent?alt=sse），
// 含工具调用、多模态输入（inlineData/FileData）、结构化输出（responseSchema）
// 与 extended thinking（thoughtsTokenCount）。模型名位于 URL path，而非请求体。
//
// wire 层（请求/响应结构体、Schema、SSE 帧解析、用量转换）复用 MoGO 的
// hegq.net/mogo/pkg/protocol/gemini 与 hegq.net/mogo/pkg/usage，
// 本包只保留统一 llm.ChatModel 抽象与协议映射逻辑。
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	pkggemini "hegq.net/mogo/pkg/protocol/gemini"
	pkgusage "hegq.net/mogo/pkg/usage"

	"hegq.net/mos/llm"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// Model 是 Gemini 协议的实现。
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
		return nil, errors.New("gemini: missing required config \"api_key\"")
	}
	if p.model == "" {
		return nil, errors.New("gemini: missing required config \"model\"")
	}
	if p.baseURL == "" {
		p.baseURL = defaultBaseURL
	}
	return p, nil
}

// Capabilities 实现 llm.ChatModel。
func (p *Model) Capabilities() llm.Capability {
	return llm.CapFunctionCalling | llm.CapStreaming |
		llm.CapStructuredOutput | llm.CapMultimodal | llm.CapReasoning
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
	body, err := json.Marshal(p.buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("gemini: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(stream), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gemini: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		var errResp pkggemini.GeminiErrorResponse
		if json.Unmarshal(data, &errResp) == nil && errResp.Error != nil && errResp.Error.Message != "" {
			return nil, fmt.Errorf("gemini: %s", errResp.Error.Message)
		}
		return nil, fmt.Errorf("gemini: unexpected status %d: %s", resp.StatusCode, string(data))
	}

	if stream {
		return p.handleStream(resp.Body, emit)
	}
	return p.handleResponse(resp.Body)
}

// endpoint 返回模型端点：模型名位于 URL path（Gemini 协议约定）。
func (p *Model) endpoint(stream bool) string {
	action := "generateContent"
	if stream {
		action = "streamGenerateContent?alt=sse"
	}
	return p.baseURL + "/models/" + p.model + ":" + action
}

func (p *Model) buildRequest(req *llm.Request) *pkggemini.GeminiGenerateContentRequest {
	gr := &pkggemini.GeminiGenerateContentRequest{
		Contents: make([]*pkggemini.GeminiContent, 0, len(req.Messages)),
	}

	var systemParts []string
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			systemParts = append(systemParts, m.Content)
		case llm.RoleUser:
			c := &pkggemini.GeminiContent{Role: pkggemini.GeminiRoleUser}
			if len(m.ContentParts) > 0 {
				c.Parts = geminiParts(m.ContentParts)
			} else {
				c.Parts = []*pkggemini.GeminiPart{{Text: m.Content}}
			}
			gr.Contents = append(gr.Contents, c)
		case llm.RoleAssistant:
			c := &pkggemini.GeminiContent{Role: pkggemini.GeminiRoleModel}
			if m.Content != "" {
				c.Parts = append(c.Parts, &pkggemini.GeminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := map[string]any{}
				if err := json.Unmarshal([]byte(tc.Args), &args); err != nil {
					args = map[string]any{"raw": tc.Args}
				}
				c.Parts = append(c.Parts, &pkggemini.GeminiPart{
					FunctionCall: &pkggemini.GeminiFunctionCall{ID: tc.ID, Name: tc.Name, Args: args},
				})
			}
			if len(c.Parts) > 0 {
				gr.Contents = append(gr.Contents, c)
			}
		case llm.RoleTool:
			// 工具结果以 functionResponse 形式作为 user 内容回传（Gemini 协议约定）。
			gr.Contents = append(gr.Contents, &pkggemini.GeminiContent{
				Role: pkggemini.GeminiRoleUser,
				Parts: []*pkggemini.GeminiPart{{
					FunctionResponse: &pkggemini.GeminiFunctionResponse{
						ID:       m.ToolCallID,
						Name:     m.Name,
						Response: resultMap(m.Content),
					},
				}},
			})
		}
	}
	if len(systemParts) > 0 {
		gr.SystemInstruction = &pkggemini.GeminiContent{
			Parts: []*pkggemini.GeminiPart{{Text: strings.Join(systemParts, "\n\n")}},
		}
	}

	if len(req.Tools) > 0 {
		decls := make([]*pkggemini.GeminiFunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, &pkggemini.GeminiFunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  toGeminiSchema(t.Parameters),
			})
		}
		gr.Tools = []*pkggemini.GeminiTool{{FunctionDeclarations: decls}}
	}

	gc := &pkggemini.GeminiGenerationConfig{}
	if req.Temperature != nil {
		t := float32(*req.Temperature)
		gc.Temperature = &t
	}
	if req.MaxTokens != nil {
		gc.MaxOutputTokens = int32(*req.MaxTokens)
	}
	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_schema":
			gc.ResponseMIMEType = "application/json"
			gc.ResponseSchema = toGeminiSchema(rf.Schema)
		case "json_object":
			gc.ResponseMIMEType = "application/json"
		}
	}
	gr.GenerationConfig = gc
	return gr
}

// geminiParts 将统一内容块展开为 Gemini Part 数组。
// image/audio 走 inlineData（base64），或 FileData（远程地址）；file 走 FileData。
func geminiParts(parts []llm.ContentPart) []*pkggemini.GeminiPart {
	out := make([]*pkggemini.GeminiPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "image", "audio":
			if p.URL != "" {
				out = append(out, &pkggemini.GeminiPart{
					FileData: &pkggemini.GeminiFileData{MIMEType: p.MIME, FileURI: p.URL},
				})
			} else {
				data, err := base64.StdEncoding.DecodeString(p.Data)
				if err != nil {
					data = []byte(p.Data)
				}
				out = append(out, &pkggemini.GeminiPart{
					InlineData: &pkggemini.GeminiBlob{MIMEType: mimeOr(p.MIME, "image/png"), Data: data},
				})
			}
		case "file":
			out = append(out, &pkggemini.GeminiPart{
				FileData: &pkggemini.GeminiFileData{MIMEType: p.MIME, FileURI: p.URL},
			})
		default: // text
			out = append(out, &pkggemini.GeminiPart{Text: p.Text})
		}
	}
	return out
}

func mimeOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// resultMap 将工具结果文本转为 functionResponse 的 response 结构：
// 能解析为 JSON 对象则原样使用，否则包装为 {"result": ...}。
func resultMap(s string) map[string]any {
	if s == "" {
		return map[string]any{"result": ""}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err == nil {
		return m
	}
	return map[string]any{"result": s}
}

// toGeminiSchema 将统一的 JSON Schema 定义（any）转换为 Gemini Schema 结构。
func toGeminiSchema(v any) *pkggemini.GeminiSchema {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case *pkggemini.GeminiSchema:
		return t
	case pkggemini.GeminiSchema:
		return &t
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var s pkggemini.GeminiSchema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return &s
}

// candidateParts 便捷访问 candidate 的 content parts。
func candidateParts(c *pkggemini.GeminiCandidate) []*pkggemini.GeminiPart {
	if c == nil || c.Content == nil {
		return nil
	}
	return c.Content.Parts
}

func (p *Model) handleResponse(r io.Reader) (*llm.Response, error) {
	var raw pkggemini.GeminiGenerateContentResponse
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("gemini: decode response: %w", err)
	}
	if len(raw.Candidates) == 0 {
		return nil, errors.New("gemini: empty candidates in response")
	}
	msg := llm.Message{Role: llm.RoleAssistant}
	for _, part := range candidateParts(raw.Candidates[0]) {
		if part.Text != "" {
			msg.Content += part.Text
		}
		if fc := part.FunctionCall; fc != nil {
			args, err := json.Marshal(fc.Args)
			if err != nil {
				args = []byte("{}")
			}
			msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
				ID:   fc.ID,
				Name: fc.Name,
				Args: string(args),
			})
		}
	}
	return &llm.Response{
		Message: msg,
		Usage:   toLLMUsage(pkggemini.ExtractGeminiUsage(raw.UsageMetadata)),
	}, nil
}

// handleStream 解析 Gemini SSE 流。Gemini 协议特殊：每个 `data:` 帧都是一个
// 完整的 GenerateContentResponse（非事件 union），文本与工具调用均为增量累积。
func (p *Model) handleStream(r io.Reader, emit func(llm.StreamChunk) error) (*llm.Response, error) {
	acc := newStreamAccumulator()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		evt, _, err, u := pkggemini.ParseGeminiStreamChunk(line)
		if err != nil {
			return nil, fmt.Errorf("gemini: parse stream frame: %w", err)
		}
		if u != nil {
			acc.usage = u // 流末尾帧通常含完整用量，逐帧覆盖
		}
		frame, _ := evt.(*pkggemini.GeminiGenerateContentResponse)
		if frame == nil || len(frame.Candidates) == 0 {
			continue
		}
		cand := frame.Candidates[0]
		for _, part := range candidateParts(cand) {
			if part.Text != "" {
				acc.content.WriteString(part.Text)
				if emit != nil {
					if err := emit(llm.StreamChunk{Delta: part.Text}); err != nil {
						return nil, err
					}
				}
			}
			if fc := part.FunctionCall; fc != nil {
				acc.mergeFunctionCall(fc)
			}
		}
		if cand.FinishReason != "" {
			if emit != nil {
				if err := emit(llm.StreamChunk{FinishReason: strings.ToLower(cand.FinishReason)}); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("gemini: read stream: %w", err)
	}

	msg := llm.Message{Role: llm.RoleAssistant, Content: acc.content.String()}
	if calls := acc.finishCalls(); len(calls) > 0 {
		msg.ToolCalls = calls
	}
	resp := &llm.Response{Message: msg}
	if acc.usage != nil {
		resp.Usage = toLLMUsage(acc.usage)
	}
	return resp, nil
}

// toLLMUsage 将 MoGO 统一用量映射为 MoS 统一结构（缓存命中 → CacheRead，思考 token → Reasoning）。
func toLLMUsage(u *pkgusage.Usage) llm.Usage {
	out := llm.Usage{}
	if u == nil {
		return out
	}
	out.PromptTokens = int(u.InputTokens)
	out.CompletionTokens = int(u.OutputTokens)
	out.TotalTokens = int(u.TotalTokens)
	if u.Cache != nil {
		out.Details.Input.CacheRead = int(u.Cache.CachedTokens)
	}
	if u.Thinking != nil {
		out.Details.Output.Reasoning = int(u.Thinking.ThinkingTokens)
	}
	return out
}

// ---- 流式聚合 ----

type streamAccumulator struct {
	content strings.Builder
	calls   map[string]*streamFunctionCall
	usage   *pkgusage.Usage
}

type streamFunctionCall struct {
	id   string
	name string
	args string
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{calls: make(map[string]*streamFunctionCall)}
}

// mergeFunctionCall 聚合工具调用增量：Gemini 流式 functionCall 的 args 为累计快照，
// 因此按 name 聚合且以最新帧为准（覆盖而非拼接）。
func (a *streamAccumulator) mergeFunctionCall(fc *pkggemini.GeminiFunctionCall) {
	key := fc.Name
	if key == "" {
		key = fc.ID
	}
	cur := a.calls[key]
	if cur == nil {
		cur = &streamFunctionCall{}
		a.calls[key] = cur
	}
	if fc.ID != "" {
		cur.id = fc.ID
	}
	if fc.Name != "" {
		cur.name = fc.Name
	}
	if len(fc.Args) > 0 {
		if b, err := json.Marshal(fc.Args); err == nil {
			cur.args = string(b)
		}
	}
}

// finishCalls 在流结束时产出完整的工具调用列表（无 name 的增量丢弃）。
func (a *streamAccumulator) finishCalls() []llm.ToolCall {
	if len(a.calls) == 0 {
		return nil
	}
	var out []llm.ToolCall
	for _, c := range a.calls {
		if c.name == "" {
			continue
		}
		out = append(out, llm.ToolCall{
			ID:   c.id,
			Name: c.name,
			Args: c.args,
		})
	}
	return out
}
