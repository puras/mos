// Package seam 提供组件注册表 SeamRegistry（架构 v2 §3.5，对齐 dsh Capability Seam）：
// 组件按 (kind, name) 注册为工厂，装配层按名解析。注册/解析是"装配时"行为，
// 不引入运行期插件热加载（Go SDK 惯用法：编译期注册 + 运行期选择）。
//
// 工厂返回 any，消费端按接口断言——注册表自身不依赖任何上层类型，保持
// 基础设施层无反向依赖（防止 agent/seam → 上层包 → agent/seam 依赖环）。
package seam

import (
	"fmt"
	"sort"
	"sync"
)

// Kind 是组件类别（driver / policy / model / tool / sandbox / memory ...）。
type Kind string

// 内置组件类别。扩展类别可直接使用自定义 Kind。
const (
	KindDriver    Kind = "driver"
	KindPolicy    Kind = "policy"
	KindModel     Kind = "model"
	KindTool      Kind = "tool"
	KindSandbox   Kind = "sandbox"
	KindMemory    Kind = "memory"
	KindRetriever Kind = "retriever"
	KindAuditor   Kind = "auditor"
	KindTruncator Kind = "truncator"
	KindCompactor Kind = "compactor"
	KindPlanner   Kind = "planner"
	KindReflector Kind = "reflector"
)

// Factory 是组件提供者：每次解析调用工厂，产出独立实例（有状态组件不共享）。
// 返回类型由消费端断言（如 driver.LoopDriver / llm.ChatModel / tool.Tool）。
type Factory func() any

// SeamRegistry 是组件注册表：按 (kind, name) 注册/解析（架构 v2 §3.5）。
// 并发安全；同名重复注册覆盖（后注册生效）。
type SeamRegistry struct {
	mu        sync.RWMutex
	providers map[Kind]map[string]Factory
}

// New 创建空注册表。
func New() *SeamRegistry {
	return &SeamRegistry{providers: make(map[Kind]map[string]Factory)}
}

// Register 注册组件工厂（kind + name 唯一，同名覆盖）。
func (s *SeamRegistry) Register(kind Kind, name string, f Factory) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byName := s.providers[kind]
	if byName == nil {
		byName = make(map[string]Factory)
		s.providers[kind] = byName
	}
	byName[name] = f
}

// Resolve 按名解析组件：调用工厂返回实例，消费端断言具体接口。
func (s *SeamRegistry) Resolve(kind Kind, name string) (any, error) {
	if s == nil {
		return nil, fmt.Errorf("seam: nil registry")
	}
	s.mu.RLock()
	f, ok := s.providers[kind][name]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("seam: %s provider %q not registered", kind, name)
	}
	return f(), nil
}

// Has 报告 (kind, name) 是否已注册。
func (s *SeamRegistry) Has(kind Kind, name string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.providers[kind][name]
	return ok
}

// Names 返回某类别下已注册的组件名（排序稳定，便于枚举）。
func (s *SeamRegistry) Names(kind Kind) []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.providers[kind]))
	for name := range s.providers[kind] {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
