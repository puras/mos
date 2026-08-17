package telemetry

import (
	"context"
	"errors"
	"testing"
)

// TestNoop 验证 no-op 实现：nil 安全、方法调用不 panic、WithSpan 透传。
func TestNoop(t *testing.T) {
	Conformance(t, NoopTelemetry)

	span, sctx := NoopTelemetry.StartSpan(context.Background(), "n", nil)
	if span != NoopSpan {
		t.Fatal("noop StartSpan 应返回共享 NoopSpan")
	}
	if sctx == nil {
		t.Fatal("noop ctx nil")
	}
}

// TestRecorder 验证参考实现：span 树、属性、事件、错误状态、幂等 End。
func TestRecorder(t *testing.T) {
	r := NewRecorder()

	root, ctx := r.StartSpan(context.Background(), SpanDriverTurn, map[string]string{"turn": "1"})
	root.SetAttribute("driver", "react")
	root.AddEvent("turn/start", map[string]string{"reason": "user"})

	child, ctx2 := r.StartSpan(ctx, SpanLLMRequest, map[string]string{"turn": "1"})
	child.SetAttribute("stream", "false")
	child.End(nil)

	grand, _ := r.StartSpan(ctx2, SpanToolExec, map[string]string{"tool": "read_file"})
	grand.End(errors.New("denied"))
	root.End(nil)

	spans := r.Spans()
	if len(spans) != 1 {
		t.Fatalf("roots = %d, want 1", len(spans))
	}
	rootRec := spans[0]
	if rootRec.Name != SpanDriverTurn || rootRec.Attributes["driver"] != "react" {
		t.Fatalf("root = %+v", rootRec)
	}
	if len(rootRec.Events) != 1 || rootRec.Events[0].Name != "turn/start" {
		t.Fatalf("events = %+v", rootRec.Events)
	}
	if rootRec.Err != nil {
		t.Fatalf("root err = %v, want nil", rootRec.Err)
	}
	if len(rootRec.Children) != 1 {
		t.Fatalf("children = %d, want 1", len(rootRec.Children))
	}
	llmRec := rootRec.Children[0]
	if llmRec.Name != SpanLLMRequest || llmRec.Err != nil {
		t.Fatalf("llm span = %+v", llmRec)
	}
	if len(llmRec.Children) != 1 {
		t.Fatalf("llm children = %d, want 1", len(llmRec.Children))
	}
	toolRec := llmRec.Children[0]
	if toolRec.Name != SpanToolExec || toolRec.Err == nil {
		t.Fatalf("tool span = %+v", toolRec)
	}
	if toolRec.Start.IsZero() || toolRec.End.IsZero() || toolRec.End.Before(toolRec.Start) {
		t.Fatalf("tool span 时间戳异常: %+v", toolRec)
	}

	// End 幂等：重复 End 不 panic、不产生新根
	root.End(nil)
	if got := len(r.Spans()); got != 1 {
		t.Fatalf("roots after re-End = %d, want 1", got)
	}
}

// TestRecorderEndBeforeEndedOps 验证 End 后 SetAttribute/AddEvent 被容忍（不 panic、不生效）。
func TestRecorderEndBeforeEndedOps(t *testing.T) {
	r := NewRecorder()
	span, _ := r.StartSpan(context.Background(), "s", nil)
	span.End(nil)
	span.SetAttribute("late", "x") // 容忍
	span.AddEvent("late-ev", nil)
	spans := r.Spans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	if _, ok := spans[0].Attributes["late"]; ok {
		t.Fatal("End 后 SetAttribute 不应生效")
	}
	if len(spans[0].Events) != 0 {
		t.Fatal("End 后 AddEvent 不应生效")
	}
}
