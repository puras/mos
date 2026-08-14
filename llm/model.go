// Package llm 提供统一的 LLM 接入抽象，支持多协议适配器（OpenAI 兼容 / Anthropic 等）。
package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Capability 表示协议能力位标志。
type Capability uint32

const (
	// CapFunctionCalling 支持函数调用（工具）。
	CapFunctionCalling Capability = 1 << iota
	// CapStreaming 支持流式输出。
	CapStreaming
	// CapStructuredOutput 支持结构化输出（response_format）。
	CapStructuredOutput
	// CapMultimodal 支持多模态输入（图片/音频内容块）。
	CapMultimodal
	// CapReasoning 支持推理模型参数（reasoning_effort）与推理 token 统计。
	CapReasoning
)

// Role 表示消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 表示模型请求调用某个工具。
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // JSON 编码的参数
}

// ContentPart 表示消息中的一个内容块，支持多模态输入（文本/图片/音频/文件）。
// 与 Message.Content 的关系：Content 是纯文本快捷方式；当 ContentParts 非空时，
// 适配器应优先将其展开为协议的多模态格式。
type ContentPart struct {
	// Type 是内容块类型：text | image | audio | file。
	Type string `json:"type"`
	// Text 是 text 类型块的文本内容。
	Text string `json:"text,omitempty"`
	// URL 是 image/audio 类型块的远程地址。
	URL string `json:"url,omitempty"`
	// Data 是 image/audio 类型块的 base64 数据。
	Data string `json:"data,omitempty"`
	// MIME 是 image/audio 类型块的媒体类型（如 image/png、audio/mpeg）。
	MIME string `json:"mime_type,omitempty"`
}

// Message 是对话中的一条消息。
type Message struct {
	Role         Role          `json:"role"`
	Content      string        `json:"content,omitempty"`
	ContentParts []ContentPart `json:"content_parts,omitempty"` // 多模态内容块，优先于 Content
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`    // 仅 assistant 消息
	ToolCallID   string        `json:"tool_call_id,omitempty"`  // 仅 tool 消息，关联 ToolCalls 中的 ID
	Name         string        `json:"name,omitempty"`          // 仅 tool 消息，工具名
	// ReasoningContent 是推理内容（思维链，DeepSeek/Qwen 等厂商扩展）。
	// 仅 assistant 消息；thinking 模式多轮对话须原样回传，否则服务端报错。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// Tool 描述一个可供模型调用的函数。
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"` // JSON Schema 对象（map[string]any 或结构体）
}

// Usage 表示一次请求的 token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Details 是提供商返回的细分明细（缓存命中/写入、推理 token 等），缺失时为零值。
	Details UsageDetails `json:"details,omitempty"`
}

// UsageDetails 是 token 用量的细分明细。
type UsageDetails struct {
	Input  InputTokenDetails  `json:"input,omitempty"`
	Output OutputTokenDetails `json:"output,omitempty"`
}

// InputTokenDetails 是输入侧 token 明细。
type InputTokenDetails struct {
	// CacheRead 是命中缓存的输入 token 数（缓存命中）。
	CacheRead int `json:"cache_read,omitempty"`
	// CacheWrite 是新写入缓存的输入 token 数（缓存未命中，创建缓存）。
	CacheWrite int `json:"cache_write,omitempty"`
	// Audio 是音频输入 token 数。
	Audio int `json:"audio,omitempty"`
}

// OutputTokenDetails 是输出侧 token 明细。
type OutputTokenDetails struct {
	// Reasoning 是推理（思维链）token 数，不直接出现在模型输出中。
	Reasoning int `json:"reasoning,omitempty"`
	// Audio 是音频输出 token 数。
	Audio int `json:"audio,omitempty"`
}

// ResponseFormat 指定模型输出的期望格式（结构化输出）。
type ResponseFormat struct {
	// Type 是格式类型：json_schema | json_object | text。
	Type string `json:"type"`
	// Name 是 json_schema 模式的名称。
	Name string `json:"name,omitempty"`
	// Schema 是 json_schema 模式定义（JSON Schema 对象或结构体）。
	Schema any `json:"schema,omitempty"`
}

// Request 是一次对话请求。
type Request struct {
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	// ResponseFormat 请求结构化输出；不支持该能力的协议应忽略。
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	// Modalities 期望的输出模态（如 text、audio、image），空表示协议默认。
	Modalities []string `json:"modalities,omitempty"`
	// ReasoningEffort 推理模型的思考投入（low | medium | high），仅支持推理能力的协议生效。
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
}

// Response 是对话响应。
type Response struct {
	Message Message `json:"message"`
	Usage   Usage   `json:"usage"`
	// FinishReason 是模型停止原因（openai: stop/length/tool_calls；anthropic: end_turn/max_tokens/tool_use；
	// gemini: STOP/MAX_TOKENS/... 已转小写）。length/max_tokens 表示输出被截断，
	// 循环层据此对未执行完的工具调用做 fail-closed 处理（见 agent.ReActDriver）。
	FinishReason string `json:"finish_reason,omitempty"`
}

// StreamChunk 是流式输出的增量块。
type StreamChunk struct {
	Delta        string // 增量文本
	ToolCall     *ToolCall
	FinishReason string
}

// ChatModel 是统一的聊天模型抽象（模型对象）。
// 仅包含对话能力（Chat/ChatStream）；后续其他能力（如 Embedding/Image）
// 以并列接口（EmbeddingModel 等）扩展，需要全能力时再组合。
type ChatModel interface {
	// Chat 发起一次非流式对话。
	Chat(ctx context.Context, req *Request) (*Response, error)
	// ChatStream 发起流式对话，增量块通过 emit 推送；emit 返回 error 可中断输出。
	// 返回值为聚合后的完整响应。
	ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error)
	// Capabilities 返回模型支持的能力位。
	Capabilities() Capability
}

// Config 是创建 ChatModel 的通用配置，键名约定：api_key / base_url / model / version 等。
type Config map[string]string

// Factory 根据配置创建 ChatModel。
type Factory func(cfg Config) (ChatModel, error)

// Registry 管理协议适配器的注册与查找。
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry 创建一个空的协议注册表。
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register 注册一个协议适配器工厂。
func (r *Registry) Register(name string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = f
}

// Create 按名称创建 ChatModel。
func (r *Registry) Create(name string, cfg Config) (ChatModel, error) {
	r.mu.RLock()
	f, ok := r.factories[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown provider %q, registered: %v", name, r.Names())
	}
	return f(cfg)
}

// Names 返回已注册的协议名称（排序后）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.factories))
	for n := range r.factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// HasCapability 判断能力位是否包含指定能力。
func (c Capability) HasCapability(cap Capability) bool {
	return c&cap != 0
}

// ErrUnsupported 表示协议不支持所请求的能力。
var ErrUnsupported = errors.New("llm: capability not supported")
