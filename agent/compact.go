package agent

import (
	"strings"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
)

// Compactor 对消息历史做 fresh-context 压缩（上下文是稀缺资源，不变式 4）。
//
// 压缩发生在决策前（每个决策轮都重算，无需持久化任何"压缩状态"）：
// 历史超过预算时，旧消息让位给一份摘要，最近的消息原样保留。摘要**不是**
// LLM 的再次总结——那是用模型的转述替代事实；它只能是 TaskState 的快照，
// 即已被环境验证过的事实的投影。
type Compactor interface {
	// Compact 压缩 history（含首条 system 时原样保留），返回压缩后的历史
	// 与是否发生了压缩（false 时返回值无效，调用方应沿用原历史）。
	Compact(history []llm.Message, state *session.TaskState) (after []llm.Message, compacted bool)
}

// DefaultKeepRecent 默认保留最近 12 条消息（约 3-4 个决策轮的工具往返）。
// 经验参数，随模型迭代可能失效（PRD R5）——可配。
const DefaultKeepRecent = 12

// CompactorConfig 是默认压缩器的配置。
type CompactorConfig struct {
	// KeepRecent 保留的最近消息条数（不含 system；<=0 取 DefaultKeepRecent）。
	// 历史超过该条数才触发压缩——预算就是触发线，不是"再多留几条"。
	KeepRecent int
	// OnCompact 是压缩发生的观测回调（可选）：summary 为注入的快照文本，
	// shadowed / kept 为被丢弃与被保留的消息条数。静默压缩是查不出问题的
	// ——长会话突然"忘了"早前的事，第一件事就是查这里。
	OnCompact func(summary string, shadowed, kept int)
}

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
//
// 切割规则：从尾部保留 KeepRecent 条；**切割点不切断 ToolCall-Result 对**
// ——保留区首条若是 tool 结果（它的 assistant 调用方落在了被丢弃区），
// 切割点前移到成对为止。投出的历史对服务商必须永远合法（投影合法性
// 不变式）：tool 消息缺了调用方会直接被服务商拒绝。
func (c *defaultCompactor) Compact(history []llm.Message, state *session.TaskState) ([]llm.Message, bool) {
	// 定位可压缩区（首条 system 引导由 History 每轮注入，原样保留）
	start := 0
	if len(history) > 0 && history[0].Role == llm.RoleSystem {
		start = 1
	}
	msgs := history[start:]
	if len(msgs) <= c.cfg.KeepRecent {
		return history, false
	}

	// 从尾部保留 KeepRecent 条；孤儿 tool 结果（调用方在切割点前）前移切割点。
	// 先定切割点、最后一次拷贝，避免循环内反复复制（O(n²)）。
	cut := len(msgs) - c.cfg.KeepRecent
	for cut > 0 && msgs[cut].Role == llm.RoleTool {
		cut--
	}
	if cut == 0 {
		// 前移后无可丢弃区（一大串工具往返连成片）：本轮放弃压缩，
		// 而不是把全部历史换成摘要——摘要替代不了没发生过的事实。
		return history, false
	}
	kept := msgs[cut:]

	out := make([]llm.Message, 0, len(kept)+2)
	if start > 0 {
		out = append(out, history[0])
	}
	summary := summaryMessage(state)
	out = append(out, summary)
	out = append(out, kept...)

	if c.cfg.OnCompact != nil {
		c.cfg.OnCompact(summary.Content, cut, len(kept))
	}
	return out, true
}

// summaryMessage 把 TaskState 快照包成一条 user 消息注入（user 角色是各家
// 协议都接受的"背景交代"位；带明确的标记头，模型知道这是框架给的进度摘要，
// 不是用户的新发言）。
func summaryMessage(state *session.TaskState) llm.Message {
	var b strings.Builder
	b.WriteString("[任务进度摘要] 此前的对话细节已省略，以下是经环境验证的进度快照：\n")
	if state != nil {
		b.WriteString(state.Snapshot())
	} else {
		b.WriteString("（暂无已验证事实）")
	}
	b.WriteString("\n[摘要结束] 若需要早前的细节，请用工具重新取证。")
	return llm.Message{Role: llm.RoleUser, Content: b.String()}
}
