package transport

import (
	"testing"

	pkgusage "github.com/puras/moproto/usage"
)

// 迁移前三个适配器各自 toLLMUsage 的行为矩阵（每协议只有自己来源字段非零），
// 固化 UsageFrom 归一化的等价性。
func TestUsageFromNil(t *testing.T) {
	got := UsageFrom(nil)
	if got.PromptTokens != 0 || got.CompletionTokens != 0 || got.TotalTokens != 0 {
		t.Fatalf("nil 用量应返回零值，got %+v", got)
	}
}

func TestUsageFromAnthropicPath(t *testing.T) {
	u := &pkgusage.Usage{}
	u.InputTokens, u.OutputTokens, u.TotalTokens = 10, 5, 15
	u.Cache = &pkgusage.CacheMetrics{ReadInputTokens: 80, CreationInputTokens: 120}
	u.Thinking = &pkgusage.ExtendedThinking{ThinkingTokens: 7}
	got := UsageFrom(u)
	if got.PromptTokens != 10 || got.CompletionTokens != 5 || got.TotalTokens != 15 {
		t.Fatalf("base = %+v", got)
	}
	if got.Details.Input.CacheRead != 80 {
		t.Fatalf("CacheRead = %d, want 80 (ReadInputTokens)", got.Details.Input.CacheRead)
	}
	if got.Details.Input.CacheWrite != 120 {
		t.Fatalf("CacheWrite = %d, want 120", got.Details.Input.CacheWrite)
	}
	if got.Details.Output.Reasoning != 7 {
		t.Fatalf("Reasoning = %d, want 7 (ThinkingTokens)", got.Details.Output.Reasoning)
	}
	if got.Details.Input.Audio != 0 || got.Details.Output.Audio != 0 {
		t.Fatalf("anthropic 路径不应有 audio，got %+v", got.Details)
	}
}

func TestUsageFromOpenAIPath(t *testing.T) {
	u := &pkgusage.Usage{}
	u.InputTokens, u.OutputTokens, u.TotalTokens = 100, 50, 150
	u.Cache = &pkgusage.CacheMetrics{CachedTokens: 60}
	u.Reasoning = &pkgusage.ReasoningTokens{ReasoningTokens: 10}
	u.Audio = &pkgusage.AudioTokens{InputAudioTokens: 3, OutputAudioTokens: 5}
	got := UsageFrom(u)
	if got.Details.Input.CacheRead != 60 {
		t.Fatalf("CacheRead = %d, want 60 (CachedTokens)", got.Details.Input.CacheRead)
	}
	if got.Details.Input.CacheWrite != 0 {
		t.Fatalf("CacheWrite = %d, want 0", got.Details.Input.CacheWrite)
	}
	if got.Details.Output.Reasoning != 10 {
		t.Fatalf("Reasoning = %d, want 10 (ReasoningTokens)", got.Details.Output.Reasoning)
	}
	if got.Details.Input.Audio != 3 || got.Details.Output.Audio != 5 {
		t.Fatalf("audio = %+v, want in=3 out=5", got.Details)
	}
}

func TestUsageFromGeminiPath(t *testing.T) {
	u := &pkgusage.Usage{}
	u.InputTokens, u.OutputTokens, u.TotalTokens = 20, 8, 28
	u.Cache = &pkgusage.CacheMetrics{CachedTokens: 10}
	u.Thinking = &pkgusage.ExtendedThinking{ThinkingTokens: 5}
	got := UsageFrom(u)
	if got.Details.Input.CacheRead != 10 {
		t.Fatalf("CacheRead = %d, want 10 (CachedTokens)", got.Details.Input.CacheRead)
	}
	if got.Details.Input.CacheWrite != 0 {
		t.Fatalf("CacheWrite = %d, want 0（gemini 无缓存写入来源）", got.Details.Input.CacheWrite)
	}
	if got.Details.Output.Reasoning != 5 {
		t.Fatalf("Reasoning = %d, want 5 (ThinkingTokens)", got.Details.Output.Reasoning)
	}
}

func TestUsageFromMaxCompatibility(t *testing.T) {
	// 生产中同协议两来源字段互斥非零；若极端情况下同时非零，max 不丢信息。
	u := &pkgusage.Usage{}
	u.Cache = &pkgusage.CacheMetrics{ReadInputTokens: 80, CachedTokens: 90}
	u.Reasoning = &pkgusage.ReasoningTokens{ReasoningTokens: 4}
	u.Thinking = &pkgusage.ExtendedThinking{ThinkingTokens: 9}
	got := UsageFrom(u)
	if got.Details.Input.CacheRead != 90 {
		t.Fatalf("CacheRead = %d, want max(80,90)=90", got.Details.Input.CacheRead)
	}
	if got.Details.Output.Reasoning != 9 {
		t.Fatalf("Reasoning = %d, want max(4,9)=9", got.Details.Output.Reasoning)
	}
}
