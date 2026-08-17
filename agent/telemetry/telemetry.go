// Package telemetry 提供显式传递的 span 契约（架构 v2 §3.11，对齐 PI pi-telemetry）：
// 无全局 current-span、无 exporter 依赖，适配器由平台注入（OpenTelemetry / 自研）。
// SDK 在驱动层自动打点（driver 回合 / llm 请求 / 工具执行），nil 时内部降级 no-op。
package telemetry

import "context"

// 驱动层自动打点的 span 名（架构 v2 §3.11：llm 请求、工具执行、driver 回合）。
const (
	SpanDriverTurn = "driver.turn"  // 一轮循环（attrs: turn / driver）
	SpanLLMRequest = "llm.request"  // 一次模型调用（attrs: turn / step / stream）
	SpanToolExec   = "tool.execute" // 一次工具执行（attrs: tool / call_id / turn）
)

// TelemetryContext 是 span 工厂：显式随 ctx 传递，应用不提供时用 NoopTelemetry。
type TelemetryContext interface {
	// StartSpan 开启一个 span，返回 span 与派生 ctx；子 span 应基于派生 ctx 开启
	// （形成 span 树）。attrs 为起始属性（nil 允许）。
	StartSpan(ctx context.Context, name string, attrs map[string]string) (TelemetrySpan, context.Context)
}

// TelemetrySpan 是一个进行中的 span。
type TelemetrySpan interface {
	// SetAttribute 设置/覆盖属性（End 后调用不保证生效，实现应容忍）。
	SetAttribute(k, v string)
	// AddEvent 记录 span 内事件（attrs 为事件属性，nil 允许）。
	AddEvent(name string, attrs map[string]string)
	// End 结束 span；err != nil 时实现应标记为错误状态。幂等。
	End(err error)
}

// NoopTelemetry 与 NoopSpan 是无操作实现：应用未提供遥测时使用，调用零开销、永不失败。
var (
	NoopTelemetry TelemetryContext = noopContext{}
	NoopSpan      TelemetrySpan    = noopSpan{}
)

type noopContext struct{}

func (noopContext) StartSpan(ctx context.Context, _ string, _ map[string]string) (TelemetrySpan, context.Context) {
	return NoopSpan, ctx
}

type noopSpan struct{}

func (noopSpan) SetAttribute(string, string)        {}
func (noopSpan) AddEvent(string, map[string]string) {}
func (noopSpan) End(error)                          {}

// WithSpan 是回调式 span 生命周期便捷函数（对齐 PI startSpan 回调形态）：
// 开启 span → 执行 fn（ctx 为派生 ctx）→ 自动 End（fn 返回错误时 span 标记错误并透传）。
// tc 为 nil 时退化为直接执行 fn（无遥测环境零成本）。
func WithSpan(ctx context.Context, tc TelemetryContext, name string, attrs map[string]string, fn func(ctx context.Context) error) error {
	if tc == nil {
		return fn(ctx)
	}
	span, sctx := tc.StartSpan(ctx, name, attrs)
	err := fn(sctx)
	span.End(err)
	return err
}
