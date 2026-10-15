package llm

import (
	"context"
	"testing"
)

// TestCapabilityHas：能力位判断。
func TestCapabilityHas(t *testing.T) {
	c := CapFunctionCalling | CapStreaming
	if !c.HasCapability(CapFunctionCalling) || !c.HasCapability(CapStreaming) {
		t.Fatal("should contain both bits")
	}
	if c.HasCapability(CapMultimodal) {
		t.Fatal("should not contain multimodal")
	}
	if c.HasCapability(0) {
		t.Fatal("zero capability should not be contained")
	}
}

// compile-time check：fakeModel 满足 ChatModel 接口。
var _ ChatModel = (*fakeModel)(nil)
var _ = context.Background
