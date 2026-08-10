package cli

import (
	"context"
	"testing"

	"hegq.net/mos/pkg/assistant"
	"hegq.net/mos/pkg/llm"
)

// denyAllPolicy 是一个自定义策略（非 DefaultPolicy），用于验证 attachConfirmer 不改动自定义策略。
type denyAllPolicy struct{}

func (denyAllPolicy) Allow(context.Context, *llm.ToolCall) error { return nil }

// attachConfirmer 按展示模式挂载确认器：
// 全屏 TUI 不挂载（受限危险操作 fail-closed 拒绝），纯文本 REPL 挂载交互确认。
func TestAttachConfirmer(t *testing.T) {
	// TUI 模式：Confirm 保持 nil → rm/sudo 等受限危险命令将被策略拒绝
	cfg := &assistant.Config{Policy: assistant.NewDefaultPolicy(nil, "/tmp")}
	attachConfirmer(cfg, true)
	if dp := cfg.Policy.(*assistant.DefaultPolicy); dp.Confirm != nil {
		t.Fatal("TUI 模式不应挂载确认器")
	}

	// 纯文本模式：挂载交互确认器 → 受限危险命令可经人工确认放行
	attachConfirmer(cfg, false)
	if dp := cfg.Policy.(*assistant.DefaultPolicy); dp.Confirm == nil {
		t.Fatal("纯文本模式应挂载确认器")
	}
}

// 非 DefaultPolicy 的自定义策略不应被 attachConfirmer 修改。
func TestAttachConfirmerCustomPolicyUntouched(t *testing.T) {
	cfg := &assistant.Config{Policy: denyAllPolicy{}}
	attachConfirmer(cfg, false)
	if _, ok := cfg.Policy.(*assistant.DefaultPolicy); ok {
		t.Fatal("自定义策略不应被替换或修改")
	}
}
