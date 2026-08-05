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
	// CapStructuredOutput 支持结构化输出。
	CapStructuredOutput
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

// Message 是对话中的一条消息。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // 仅 assistant 消息
	ToolCallID string     `json:"tool_call_id,omitempty"` // 仅 tool 消息，关联 ToolCalls 中的 ID
	Name       string     `json:"name,omitempty"`         // 仅 tool 消息，工具名
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
}

// Request 是一次对话请求。
type Request struct {
	Messages    []Message
	Tools       []Tool
	Temperature *float64
	MaxTokens   *int
}

// Response 是对话响应。
type Response struct {
	Message Message
	Usage   Usage
}

// StreamChunk 是流式输出的增量块。
type StreamChunk struct {
	Delta        string // 增量文本
	ToolCall     *ToolCall
	FinishReason string
}

// Model 是统一的 LLM 模型抽象（模型对象）。
type Model interface {
	// Chat 发起一次非流式对话。
	Chat(ctx context.Context, req *Request) (*Response, error)
	// ChatStream 发起流式对话，增量块通过 emit 推送；emit 返回 error 可中断输出。
	// 返回值为聚合后的完整响应。
	ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error)
	// Capabilities 返回模型支持的能力位。
	Capabilities() Capability
}

// Config 是创建 Model 的通用配置，键名约定：api_key / base_url / model / version 等。
type Config map[string]string

// Factory 根据配置创建 Model。
type Factory func(cfg Config) (Model, error)

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

// Create 按名称创建 Model。
func (r *Registry) Create(name string, cfg Config) (Model, error) {
	r.mu.RLock()
	f, ok := r.factories[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown model %q, registered: %v", name, r.Names())
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
