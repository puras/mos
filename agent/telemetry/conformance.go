package telemetry

import (
	"context"
	"errors"
	"testing"
)

// Conformance 验证 TelemetryContext 实现满足契约（对齐 PI createTelemetryAdapterConformance）：
//  1. StartSpan 返回非 nil span 与派生 ctx；
//  2. span 方法（SetAttribute / AddEvent / End）可安全调用（no-op 亦不 panic），End 幂等；
//  3. WithSpan 回调恰好执行一次、错误透传、自动 End（End 后再调用方法不 panic）；
//  4. 嵌套 span 基于派生 ctx 可正常开启（span 树）。
//
// 平台实现适配器（OpenTelemetry / 自研）后，在自身测试中调用本函数验证契约，
// 再结合自己的快照断言属性 / 事件 / 树结构。
func Conformance(t *testing.T, tc TelemetryContext) {
	t.Helper()
	if tc == nil {
		t.Fatal("telemetry: Conformance: nil TelemetryContext")
	}

	t.Run("start-span", func(t *testing.T) {
		span, sctx := tc.StartSpan(context.Background(), "test.span", map[string]string{"k": "v"})
		if span == nil {
			t.Fatal("StartSpan 返回 nil span")
		}
		if sctx == nil {
			t.Fatal("StartSpan 返回 nil ctx")
		}
		// 派生 ctx 可用于开启子 span（span 树），不应 panic
		child, _ := tc.StartSpan(sctx, "test.child", nil)
		if child == nil {
			t.Fatal("嵌套 StartSpan 返回 nil span")
		}
		child.SetAttribute("a", "b")
		child.AddEvent("ev", nil)
		child.End(nil)
		span.End(nil)
	})

	t.Run("with-span-success", func(t *testing.T) {
		calls := 0
		err := WithSpan(context.Background(), tc, "test.with", nil, func(ctx context.Context) error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("WithSpan err = %v, want nil", err)
		}
		if calls != 1 {
			t.Fatalf("callback calls = %d, want 1", calls)
		}
	})

	t.Run("with-span-error", func(t *testing.T) {
		sentinel := errors.New("boom")
		got := WithSpan(context.Background(), tc, "test.err", nil, func(context.Context) error {
			return sentinel
		})
		if !errors.Is(got, sentinel) {
			t.Fatalf("WithSpan err = %v, want sentinel", got)
		}
	})

	t.Run("with-span-nil-tc", func(t *testing.T) {
		calls := 0
		if err := WithSpan(context.Background(), nil, "test.nil", nil, func(context.Context) error {
			calls++
			return nil
		}); err != nil {
			t.Fatalf("WithSpan(nil tc) err = %v", err)
		}
		if calls != 1 {
			t.Fatalf("callback calls = %d, want 1", calls)
		}
	})
}

// TestConformance 验证契约对 no-op 与参考实现均成立（平台适配器应复用 Conformance）。
func TestConformance(t *testing.T) {
	t.Run("noop", func(t *testing.T) { Conformance(t, NoopTelemetry) })
	t.Run("recorder", func(t *testing.T) { Conformance(t, NewRecorder()) })
}
