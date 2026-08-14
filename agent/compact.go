package agent

import "hegq.net/mos/llm"

// Compactor 对消息历史做 fresh-context 压缩（M2.5 上下文工程）。
// 压缩发生在两轮之间：历史超过阈值时，将旧消息压缩为摘要注入，保留最近消息，
// 防止跨轮累积的上下文腐化（40% Dumb Zone 之后质量陡降）。
type Compactor interface {
	// Compact 对 history 做压缩，返回压缩后的消息列表。
	// 第二返回值表示是否发生了压缩（false 时第一返回值无效）。
	// 压缩摘要直接采用审计写入的 TaskState.Snapshot()，而非 LLM 重新总结——
	// 已被环境独立验证过的事实才进入压缩摘要，模型自述不算数。
	Compact(history []InternalMessage, state *TaskState) ([]InternalMessage, bool)
}

// CompactorConfig 是默认压缩器的配置。
type CompactorConfig struct {
	// KeepRecent 保留的最近消息条数；历史（不含 system）超过该阈值时触发压缩。
	KeepRecent int
}

// DefaultKeepRecent 默认保留最近 12 条消息（约 3-4 个 Turn）。
const DefaultKeepRecent = 12

// NewDefaultCompactor 创建默认压缩器。
func NewDefaultCompactor(cfg CompactorConfig) Compactor {
	if cfg.KeepRecent <= 0 {
		cfg.KeepRecent = DefaultKeepRecent
	}
	return &defaultCompactor{cfg: cfg}
}

type defaultCompactor struct {
	cfg CompactorConfig
}

// Compact 实现 Compactor。
// 切割规则：从尾部保留 KeepRecent 条；切割点不切断 ToolCall-Result 对——
// 保留区首条若是 tool 结果（其 assistant 调用被切），前移切割点使成对保留。
func (c *defaultCompactor) Compact(history []InternalMessage, state *TaskState) ([]InternalMessage, bool) {
	// 定位可压缩区（排除开头的 system 引导，system 由 Agent 每轮注入）
	start := 0
	if len(history) > 0 && history[0].Role == llm.RoleSystem {
		start = 1
	}
	msgs := history[start:]
	if len(msgs) <= c.cfg.KeepRecent {
		return history, false
	}

	// 从尾部保留 KeepRecent 条；孤儿 tool 结果（assistant 调用在切割点前）前移切割点。
	// 先移动 cut 指针、最后一次拷贝，避免循环内反复复制 kept 切片（O(n²)）。
	cut := len(msgs) - c.cfg.KeepRecent
	for cut > 0 && msgs[cut].Role == llm.RoleTool {
		cut--
	}
	kept := msgs[cut:]

	// 摘要注入：采用审计写入的 TaskState（已验证事实），经翻译边界转 user 角色注入
	summary := SummaryMsg(state.Snapshot())

	out := make([]InternalMessage, 0, len(kept)+2)
	if start > 0 {
		out = append(out, history[0])
	}
	out = append(out, summary)
	out = append(out, kept...)
	return out, true
}
