package transport

import (
	"github.com/puras/mos/llm"

	pkgusage "github.com/puras/moproto/usage"
)

// UsageFrom 将 moproto 统一用量映射为 MoS llm.Usage，收编三个适配器各自的
// toLLMUsage 并归一字段命名差异。
//
// moproto 的 *pkgusage.Usage 是协议无关的统一类型，每个协议的 FromXxx 只填充
// 自己协议的字段、其余为零值，因此语义冲突字段（缓存命中的
// ReadInputTokens vs CachedTokens、推理的 ThinkingTokens vs ReasoningTokens）
// 取 max 即无损等价于"取非零的那个"。
func UsageFrom(u *pkgusage.Usage) llm.Usage {
	out := llm.Usage{}
	if u == nil {
		return out
	}
	out.PromptTokens = int(u.InputTokens)
	out.CompletionTokens = int(u.OutputTokens)
	out.TotalTokens = int(u.TotalTokens)
	if u.Cache != nil {
		// Anthropic 填 ReadInputTokens，OpenAI/Gemini 填 CachedTokens，互斥非零。
		out.Details.Input.CacheRead = max(int(u.Cache.ReadInputTokens), int(u.Cache.CachedTokens))
		out.Details.Input.CacheWrite = int(u.Cache.CreationInputTokens)
	}
	if u.Reasoning != nil { // OpenAI o 系列 / DeepSeek
		out.Details.Output.Reasoning = int(u.Reasoning.ReasoningTokens)
	}
	if u.Thinking != nil { // Anthropic / Gemini extended thinking
		out.Details.Output.Reasoning = max(out.Details.Output.Reasoning, int(u.Thinking.ThinkingTokens))
	}
	if u.Audio != nil { // OpenAI audio
		out.Details.Input.Audio = int(u.Audio.InputAudioTokens)
		out.Details.Output.Audio = int(u.Audio.OutputAudioTokens)
	}
	return out
}
