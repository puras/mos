package react

import (
	"context"
	"testing"

	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/agent/telemetry"
	"hegq.net/mos/tool"
)

// findSpan 递归查找指定名字的 span 记录。
func findSpan(spans []telemetry.SpanRecord, name string) *telemetry.SpanRecord {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
		if c := findSpanPtr(spans[i].Children, name); c != nil {
			return c
		}
	}
	return nil
}

func findSpanPtr(spans []*telemetry.SpanRecord, name string) *telemetry.SpanRecord {
	for _, s := range spans {
		if s.Name == name {
			return s
		}
		if c := findSpanPtr(s.Children, name); c != nil {
			return c
		}
	}
	return nil
}

// TestRunTelemetrySpans 验证驱动自动打点（架构 v2 §3.11）：
// driver.turn → 子 span llm.request（每轮模型调用）+ tool.execute（工具执行，含属性）。
func TestRunTelemetrySpans(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(readFile{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	rec := telemetry.NewRecorder()
	a := newReAct(mockModel{}, reg, func(rt *driver.RunContext) {
		rt.Telemetry = rec
	})

	sess := session.NewSession("telemetry")
	if _, err := a.Run(context.Background(), sess, "1+2=3?"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := rec.Spans()
	turn := findSpan(spans, telemetry.SpanDriverTurn)
	if turn == nil {
		t.Fatalf("缺少 driver.turn span，spans = %+v", spans)
	}
	if turn.Attributes["turn"] != "1" || turn.Attributes["driver"] != "react" {
		t.Fatalf("driver.turn attrs = %v", turn.Attributes)
	}
	if turn.Err != nil {
		t.Fatalf("driver.turn err = %v", turn.Err)
	}

	llmSpan := findSpan(spans, telemetry.SpanLLMRequest)
	if llmSpan == nil {
		t.Fatal("缺少 llm.request span")
	}
	if llmSpan.Attributes["turn"] != "1" || llmSpan.Attributes["stream"] != "false" {
		t.Fatalf("llm.request attrs = %v", llmSpan.Attributes)
	}

	toolSpan := findSpan(spans, telemetry.SpanToolExec)
	if toolSpan == nil {
		t.Fatal("缺少 tool.execute span")
	}
	if toolSpan.Attributes["tool"] != "read_file" || toolSpan.Attributes["call_id"] != "c1" {
		t.Fatalf("tool.execute attrs = %v", toolSpan.Attributes)
	}
	if toolSpan.Err != nil {
		t.Fatalf("tool.execute err = %v", toolSpan.Err)
	}
	// 结构：llm.request 与 tool.execute 应挂在 driver.turn 子树内（显式 ctx 传递，无全局 current-span）
	if !isDescendant(turn, llmSpan) || !isDescendant(turn, toolSpan) {
		t.Fatal("span 树关系错误：llm/tool span 不在 driver.turn 子树内")
	}
}

// isDescendant 报告 child 是否在 root 子树内。
func isDescendant(root, child *telemetry.SpanRecord) bool {
	for _, c := range root.Children {
		if c == child || isDescendant(c, child) {
			return true
		}
	}
	return false
}

// TestRunTelemetryNil 验证未注入 Telemetry 时降级 no-op（行为不变、不 panic）。
func TestRunTelemetryNil(t *testing.T) {
	reg := tool.NewRegistry()
	_ = reg.Register(readFile{})
	a := newReAct(mockModel{}, reg, nil) // rt.Telemetry = nil

	sess := session.NewSession("nil-tel")
	reply, err := a.Run(context.Background(), sess, "1+2=3?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "结果是 3" {
		t.Fatalf("reply = %q", reply)
	}
}
