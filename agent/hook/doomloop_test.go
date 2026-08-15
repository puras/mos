package hook

import (
	"testing"

	"hegq.net/mos/llm"
)

func TestDoomLoopDetect(t *testing.T) {
	d := newDoomLoopDetector(3)
	call := &llm.ToolCall{Name: "grep", Args: `{"p":"x"}`}
	if d.observe(call) {
		t.Fatalf("第 1 次不应判定")
	}
	if d.observe(call) {
		t.Fatalf("第 2 次不应判定")
	}
	if !d.observe(call) {
		t.Fatalf("第 3 次相同调用应判定 Doom Loop")
	}
}

func TestDoomLoopDetectResetsOnDiffArgs(t *testing.T) {
	d := newDoomLoopDetector(3)
	// 连续 2 次 x：未判定
	if d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"x"}`}) {
		t.Fatalf("x 第 1 次不应判定")
	}
	if d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"x"}`}) {
		t.Fatalf("x 第 2 次不应判定")
	}
	// 参数变化重置计数：y 第 1、2 次均不判定
	if d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"y"}`}) {
		t.Fatalf("y 第 1 次（参数变化重置）不应判定")
	}
	if d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"y"}`}) {
		t.Fatalf("y 第 2 次不应判定")
	}
	// y 第 3 次连续相同 → 判定
	if !d.observe(&llm.ToolCall{Name: "grep", Args: `{"p":"y"}`}) {
		t.Fatalf("y 第 3 次连续相同应判定")
	}
}
