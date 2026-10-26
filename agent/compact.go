package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
)

// Compactor 是上下文压缩器（PI 的 compaction 契约对位）：
//
//	超预算（ShouldCompact）→ 定切割点（Plan）→ 模型生成摘要（Summarize）
//	→ 调用方把摘要作为 compaction 事件落会话日志 → 投影遮蔽旧历史。
//
// 切割点永不落在 tool 结果上（与调用方成对——三家共识的不变式）；
// 摘要由**模型**按结构化清单生成（Goal/Progress/Key Decisions/Next Steps），
// 迭代压缩时把旧摘要作为 previous-summary 传回让它更新而非重写。
type Compactor interface {
	// ShouldCompact 报告给定上下文规模是否超过预算。
	ShouldCompact(contextTokens int) bool
	// Plan 在投影视图上定切割方案：Cut 是保留段首条消息在 view 中的下标
	//（其 Seq 即压缩事件的 FirstKeptSeq），Previous 是被折叠的旧摘要文本
	//（无则空）。ok=false 表示无需或无法压缩（未超预算 / 无可摘要段）。
	Plan(view []session.SeqMessage, contextTokens int) (plan CutPlan, ok bool)
	// Summarize 把被压缩的消息序列写成结构化摘要；previous 非空时走
	// "更新既有摘要"提示词（PI 的 UPDATE_SUMMARIZATION 路径）。
	// usage 是这次摘要调用的用量（随压缩事件落日志、计入会话总量）。
	Summarize(ctx context.Context, msgs []llm.Message, previous string) (text string, usage llm.Usage, err error)
}

// CutPlan 是一次压缩切割方案。
type CutPlan struct {
	// Cut 是保留段首条消息在 view 中的下标（view[Cut:] 保留）。
	Cut int
	// Previous 是 view[:Cut] 中检出的旧摘要文本（迭代压缩的上下文）。
	Previous string
}

// LLMCompactor 是基于模型调用的默认压缩器（PI compaction.ts 的移植）。
type LLMCompactor struct {
	model llm.ChatModel
	cfg   CompactorConfig
}

// CompactorConfig 是 LLMCompactor 的配置（零值字段取 PI 的默认值）。
type CompactorConfig struct {
	// ContextWindow 是模型的上下文窗口（token）。<=0 时**永不自动触发**——
	// 不知道预算就无法负责任地压缩（fail-closed），手动路径仍可用。
	ContextWindow int
	// ReserveTokens 是为模型响应预留的空间（默认 16384，PI 同款）：
	// 超过 contextWindow-reserveTokens 即触发压缩。
	ReserveTokens int
	// KeepRecentTokens 是压缩后保留的近期上下文预算（默认 20000，PI 同款）：
	// 切割点从尾部往前按 token 估算累计到该值为止。
	KeepRecentTokens int
	// MaxToolResultChars 是序列化被压缩历史时单条工具结果的截断长度
	//（默认 2000，PI 的 TOOL_RESULT_MAX_CHARS）：摘要不需要全文。
	MaxToolResultChars int
}

// PI 的默认值（compaction.ts 的 DEFAULT_COMPACTION_SETTINGS）。
const (
	DefaultReserveTokens      = 16384
	DefaultKeepRecentTokens   = 20000
	DefaultMaxToolResultChars = 2000
)

// NewLLMCompactor 创建默认压缩器。model 为 nil 时 Summarize 报错（fail-closed）。
func NewLLMCompactor(model llm.ChatModel, cfg CompactorConfig) *LLMCompactor {
	if cfg.ReserveTokens <= 0 {
		cfg.ReserveTokens = DefaultReserveTokens
	}
	if cfg.KeepRecentTokens <= 0 {
		cfg.KeepRecentTokens = DefaultKeepRecentTokens
	}
	if cfg.MaxToolResultChars <= 0 {
		cfg.MaxToolResultChars = DefaultMaxToolResultChars
	}
	return &LLMCompactor{model: model, cfg: cfg}
}

// ShouldCompact 实现 Compactor：contextTokens > contextWindow - reserveTokens
// （PI 的 shouldCompact）。ContextWindow 未配置时恒 false（无法负责任地触发）。
func (c *LLMCompactor) ShouldCompact(contextTokens int) bool {
	if c == nil || c.cfg.ContextWindow <= 0 {
		return false
	}
	return contextTokens > c.cfg.ContextWindow-c.cfg.ReserveTokens
}

// Plan 实现 Compactor：PI 的 findCutPoint 移植。
//
// 从尾部往前按 chars/4 估算累计消息 token，累计到 KeepRecentTokens 为止；
// 切割点取**该位置之后最近的合法切割点**（user / assistant 消息——绝不
// 是 tool 结果，否则保留段开头就是孤儿回执，投影对服务商非法）。走完全程
// 都没超预算、或切割点落在开头（没有可摘要段）时返回 ok=false。
func (c *LLMCompactor) Plan(view []session.SeqMessage, _ int) (CutPlan, bool) {
	if len(view) == 0 {
		return CutPlan{}, false
	}
	// 合法切割点：user / assistant 消息的下标（0 除外——那是无可摘要段）。
	valid := make([]int, 0, len(view))
	for i, sm := range view {
		if i > 0 && (sm.Message.Role == llm.RoleUser || sm.Message.Role == llm.RoleAssistant) {
			valid = append(valid, i)
		}
	}
	if len(valid) == 0 {
		return CutPlan{}, false
	}

	accumulated := 0
	cut := -1
	for i := len(view) - 1; i >= 0; i-- {
		tokens := estimateMessageTokens(view[i].Message)
		if tokens == 0 {
			continue
		}
		accumulated += tokens
		if accumulated >= c.cfg.KeepRecentTokens {
			// 取该位置之后最近的合法切割点（保留段多一点没事，
			// 少一点就会把成对的历史切散）。
			for _, v := range valid {
				if v >= i {
					cut = v
					break
				}
			}
			if cut < 0 {
				cut = valid[len(valid)-1]
			}
			break
		}
	}
	if cut <= 0 {
		return CutPlan{}, false // 没攒到预算 / 无可摘要段
	}
	// 检出旧摘要（迭代压缩的 previous-summary，PI 同款）。
	previous := ""
	for _, sm := range view[:cut] {
		if sm.Message.Role == llm.RoleUser && strings.HasPrefix(sm.Message.Content, session.CompactionSummaryPrefix) {
			previous = strings.TrimPrefix(strings.TrimPrefix(sm.Message.Content, session.CompactionSummaryPrefix), "\n")
			break
		}
	}
	return CutPlan{Cut: cut, Previous: previous}, true
}

// Summarize 实现 Compactor：PI 的 generateSummaryWithUsage 移植——
// 序列化被压缩历史（工具结果截到 MaxToolResultChars、思维链省略），包进
// <conversation> 标签，有旧摘要时附 <previous-summary> 并换用"更新"提示词；
// 模型若试图调用工具则直接报错（摘要是纯文本产出）。
func (c *LLMCompactor) Summarize(ctx context.Context, msgs []llm.Message, previous string) (string, llm.Usage, error) {
	if c == nil || c.model == nil {
		return "", llm.Usage{}, fmt.Errorf("compactor: 未装配模型（fail-closed）")
	}

	var b strings.Builder
	b.WriteString("<conversation>\n")
	b.WriteString(serializeConversation(msgs, c.cfg.MaxToolResultChars))
	b.WriteString("\n</conversation>\n\n")
	prompt := summarizationPrompt
	if previous != "" {
		b.WriteString("<previous-summary>\n" + previous + "\n</previous-summary>\n\n")
		prompt = updateSummarizationPrompt
	}
	b.WriteString(prompt)

	resp, err := c.model.Chat(ctx, &llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: summarizationSystemPrompt},
		{Role: llm.RoleUser, Content: b.String()},
	}})
	if err != nil {
		return "", llm.Usage{}, fmt.Errorf("compactor: 摘要调用失败: %w", err)
	}
	if len(resp.Message.ToolCalls) > 0 {
		return "", llm.Usage{}, fmt.Errorf("compactor: 摘要调用试图调用工具（摘要只能是文本）")
	}
	return resp.Message.Content, resp.Usage, nil
}

// estimateMessageTokens 估算单条消息的 token（chars/4，PI 的 estimateTokens
// 同款启发式；工具调用按 名字+参数 计）。
func estimateMessageTokens(m llm.Message) int {
	n := len(m.Content) + len(m.ReasoningContent)
	for _, tc := range m.ToolCalls {
		n += len(tc.Name) + len(tc.Args)
	}
	return (n + 3) / 4
}

// extractSummary 已内联进 Plan（检出旧摘要即从序列化输入中排除——它作为
// previous-summary 单独传给模型，PI：迭代压缩不把旧摘要当普通对话序列化）。

// serializeConversation 把消息序列序列化为纯文本（PI 的 serializeConversation
// 同构）：模型据此知道这是要总结的记录，不是要续写的对话。
//
//	[User]: …
//	[Assistant]: …
//	[Assistant tool calls]: read_file(path="a.go"); …
//	[Tool result]: …（超长截到 maxChars）
//
// 思维链一律省略：被压缩的历史里，思维链通常已不在模型的活跃上下文中，
// 带上只会让摘要请求比它要总结的上下文还大。
func serializeConversation(msgs []llm.Message, maxChars int) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			if m.Content != "" {
				parts = append(parts, "[User]: "+m.Content)
			}
		case llm.RoleAssistant:
			if m.Content != "" {
				parts = append(parts, "[Assistant]: "+m.Content)
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]string, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					calls = append(calls, tc.Name+"("+string(tc.Args)+")")
				}
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
			}
		case llm.RoleTool:
			if m.Content != "" {
				parts = append(parts, "[Tool result]: "+truncateForSummary(m.Content, maxChars))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// truncateForSummary 按 rune 截断并标注省略量（PI 的 truncateForSummary 同款）。
func truncateForSummary(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars]) + fmt.Sprintf("…[%d chars truncated]", len(r)-maxChars)
}

// 提示词逐字移植自 PI 的 compaction.ts（SUMMARIZATION_SYSTEM_PROMPT /
// SUMMARIZATION_PROMPT / UPDATE_SUMMARIZATION_PROMPT）：结构化清单保证摘要
// 可吊点检索，"Preserve exact file paths..." 守住细节。
const summarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const summarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

const updateSummarizationPrompt = `The messages above are a conversation to summarize, and there is also an existing summary from a previous compaction. Update the existing summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]`
