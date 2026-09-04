package llm

import (
	"fmt"
	"sort"
	"sync"
)

// 协议注册表（Registry / Factory / Config）。
//
// 装配方按名注册 Factory、按配置创建 ChatModel；注册表只做名字到工厂的
// 映射，不感知协议差异。与 wrap.go 的装饰器分工：注册表产出裸模型，
// 横切能力（日志/重试/超时等）由调用方按需包装。

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
