// Package router 提供任务路由：按输入选择 LoopDriver（架构 v2 §3.6）。
// HybridRouter 规则（确定性/可审计）优先 + LLM 分类兜底（模糊输入），
// 返回的驱动为裸实例：Run 时经 RunContext 惰性完成默认装配。
package router

import (
	"context"
	"fmt"
	"strings"

	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/driver/planexecute"
	"hegq.net/mos/agent/driver/react"
	"hegq.net/mos/agent/driver/reflection"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/planner"
)

// DriverRouter 按任务输入选择 LoopDriver。
type DriverRouter interface {
	Select(ctx context.Context, input string, sess *session.Session) (driver.LoopDriver, error)
}

// DriverFactory 构造一个驱动实例。Router 每次 Select 经工厂产出独立实例，
// 避免共享实例的并发写（如 PlanExecuteDriver 惰性装配 executor）。
type DriverFactory func() driver.LoopDriver

// Rule 是确定性路由规则：输入包含 Keyword（大小写不敏感）时选择 Driver。
type Rule struct {
	Keyword string
	Driver  string // 驱动名（见 planner.Driver* 常量）
}

// HybridRouter 是规则优先 + LLM 兜底的混合路由：
//  1. 规则硬路由（确定性/可审计）：关键词命中即选择；
//  2. LLM 分类器（模糊输入）：未命中规则时按意图分类；
//  3. fallback（默认 react）：分类器未配置或无法判断时。
type HybridRouter struct {
	drivers    map[string]DriverFactory
	rules      []Rule
	classifier planner.Classifier
	fallback   string
}

// Option 是 HybridRouter 的装配选项。
type Option func(*HybridRouter)

// WithDriver 注册/覆盖驱动名 → 实例（自定义驱动由调用方保证并发安全）。
func WithDriver(name string, d driver.LoopDriver) Option {
	return func(r *HybridRouter) { r.drivers[name] = func() driver.LoopDriver { return d } }
}

// WithFactory 注册/覆盖驱动名 → 构造器（每次 Select 产出独立实例）。
func WithFactory(name string, f DriverFactory) Option {
	return func(r *HybridRouter) { r.drivers[name] = f }
}

// WithRule 追加确定性规则（按注册顺序匹配，先命中先选）。
func WithRule(keyword, driverName string) Option {
	return func(r *HybridRouter) {
		r.rules = append(r.rules, Rule{Keyword: keyword, Driver: driverName})
	}
}

// WithClassifier 注入 LLM 分类器（未配置时直接走 fallback）。
func WithClassifier(c planner.Classifier) Option {
	return func(r *HybridRouter) { r.classifier = c }
}

// WithFallback 设置兜底驱动名（默认 react）。
func WithFallback(name string) Option {
	return func(r *HybridRouter) { r.fallback = name }
}

// NewHybrid 创建混合路由：内置 react / plan-execute / reflection 三种驱动，
// 兜底 react，预置"先规划"显式指令规则 → plan-execute。
func NewHybrid(opts ...Option) *HybridRouter {
	r := &HybridRouter{
		drivers: map[string]DriverFactory{
			planner.DriverReact:      func() driver.LoopDriver { return &react.ReActDriver{} },
			planner.DriverPlanExec:   func() driver.LoopDriver { return &planexecute.PlanExecuteDriver{} },
			planner.DriverReflection: func() driver.LoopDriver { return &reflection.ReflectionDriver{} },
		},
		rules: []Rule{
			{Keyword: "先规划", Driver: planner.DriverPlanExec},
		},
		fallback: planner.DriverReact,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Select 实现 DriverRouter：规则 → 分类器 → fallback。
func (r *HybridRouter) Select(ctx context.Context, input string, sess *session.Session) (driver.LoopDriver, error) {
	lower := strings.ToLower(input)
	for _, rule := range r.rules {
		if strings.Contains(lower, strings.ToLower(rule.Keyword)) {
			return r.build(rule.Driver)
		}
	}
	if r.classifier != nil {
		if name, err := r.classifier.Classify(ctx, input, session.ConvertToLlm(sess.Messages())); err == nil && name != "" {
			return r.build(name)
		}
	}
	return r.build(r.fallback)
}

// build 按名构造驱动实例。
func (r *HybridRouter) build(name string) (driver.LoopDriver, error) {
	f, ok := r.drivers[name]
	if !ok {
		return nil, fmt.Errorf("router: unknown driver %q", name)
	}
	return f(), nil
}
