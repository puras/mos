// Package session 提供会话事件日志：append-only 事件是唯一事实来源，
// 消息历史由投影派生（不变式「日志即事实」）。
//
// 对话值类型（Message / ToolCall / Usage）直接采用 llm 包的定义——它们是
// 全框架共享的对话词汇表（纯定义、零依赖），日志与协议同形可避免翻译边界
// 丢字段（如 thinking 模式须回传的 ReasoningContent）。ToolResult 保留在本包：
// 它是 tool/result 事件的审计载荷，IsError 为事件日志特有语义。
//
// 最小事件目录（预算制：新增一种事件前先删一种）：
//
//	turn/start  一轮开始
//	turn/end    一轮结束（reason: completed / terminated / error / interrupted；
//	            error/terminated 另带 Error 详情）
//	user        用户输入（投影为 user 消息）
//	assistant   模型输出（文本 + 可选工具调用；投影为 assistant 消息）
//	tool/call   工具调用留痕（含被守卫拦截的调用；不投影，仅审计）
//	tool/result 工具结果（投影为 tool 消息）
//	compaction  压缩落点（S6）：旧历史由摘要遮蔽——摘要进投影，被遮蔽的
//	            事件留在日志里不删，重放确定性地复现压缩后视图（对齐 PI 的
//	            CompactionEntry 与 DSH 的 marker 消息）。消费者=投影+审计，
//	            非预留。
//	approval/asked   问了人工（审批请求发出；不投影，仅审计）
//	approval/decided 人工答了（与 asked 用同一个 id 配对；不投影，仅审计）
//
// 最后两种是 PRD 点名的例外（§可观测「approval/* 等事件全量落会话日志」、
// G4「审批挂起-恢复 E2E」）：审批的审计链断在这里就不知道谁批的。
package session

import (
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/puras/mos/llm"
)

// EventKind 是事件类型（见包注释目录）。
type EventKind string

const (
	EventTurnStart  EventKind = "turn/start"
	EventTurnEnd    EventKind = "turn/end"
	EventUser       EventKind = "user"
	EventAssistant  EventKind = "assistant"
	EventToolCall   EventKind = "tool/call"
	EventToolResult EventKind = "tool/result"
	// EventCompaction 是压缩落点：从此处起，FirstKeptSeq 之前的历史被
	// Summary 遮蔽（投影只发摘要 + 保留段），事件本身留在日志可审计。
	EventCompaction EventKind = "compaction"
	// EventApprovalAsked 是「问了人工」：审批请求发出时记一条。
	EventApprovalAsked EventKind = "approval/asked"
	// EventApprovalDecided 是「人工答了」：审批有结果时记一条（与 asked 用
	// 同一个 id 配对）。
	//
	// 这两个事件是 PRD 点名的（§可观测「approval/* 等事件全量落会话日志」、
	// G4「审批挂起-恢复 E2E」），不走事件目录的预算制。
	EventApprovalDecided EventKind = "approval/decided"
)

// Approval 是审批事件载荷（approval/asked 与 approval/decided）。
type Approval struct {
	// ID 是审批请求标识（配对 asked 与 decided）。
	ID string `json:"id"`
	// ToolName 是被审批的工具名。
	ToolName string `json:"toolName"`
	// CallID 是被审批的那次调用。
	CallID string `json:"callId,omitempty"`
	// Reason 是问人的理由（asked 才有）。
	Reason string `json:"reason,omitempty"`
	// Outcome 是审批结果（decided 才有）：approved / rejected / cancelled /
	// unavailable。
	Outcome string `json:"outcome,omitempty"`
}

// ToolResult 是一次工具执行的回填（事件日志审计载荷；
// IsError 记录显式失败——守卫拦截或工具报错，协议消息无此语义）。
type ToolResult struct {
	CallID  string `json:"callId"`
	Content string `json:"content"`
	IsError bool   `json:"isError,omitempty"`
}

// Compaction 是压缩事件载荷：旧历史被 Summary 遮蔽，FirstKeptSeq 之后的
// 事件照常投影。对齐 PI 的 CompactionEntry（summary + firstKeptEntryId +
// tokensBefore + usage）：摘要调用的用量记在事件的 Usage 上，会话总量
// 一并折叠（PI："session totals include summarization work"）。
type Compaction struct {
	// Summary 是模型生成的结构化摘要文本（进投影，user 角色）。
	Summary string `json:"summary"`
	// FirstKeptSeq 是保留段首条事件的序号：投影丢弃此前的可投影消息。
	FirstKeptSeq int64 `json:"firstKeptSeq"`
	// TokensBefore 是压缩前的上下文 token 估算（审计用）。
	TokensBefore int `json:"tokensBefore,omitempty"`
}

// CompactionSummaryPrefix 是投影注入摘要消息时的标记前缀；压缩器靠它
// 识别旧摘要（迭代压缩时作为 previous-summary 传给模型，PI 同款）。
const CompactionSummaryPrefix = "[历史摘要]"

// Event 是日志中的一条记录。Seq 由 Session.Append 统一分配，全局递增从 1 开始。
// 载荷字段按 Kind 取用：Text（user/assistant）、Reasoning（assistant，思维链）、
// ToolCalls（assistant）、Usage（assistant，模型调用遥测）、ToolCall（tool/call）、
// ToolResult（tool/result）、Reason 与 Error（turn/end）、Approval（approval/*）。
type Event struct {
	Seq        int64          `json:"seq"`
	Kind       EventKind      `json:"kind"`
	Turn       int            `json:"turn,omitempty"`
	Time       time.Time      `json:"time"`
	Text       string         `json:"text,omitempty"`
	Reasoning  string         `json:"reasoning,omitempty"`
	ToolCalls  []llm.ToolCall `json:"toolCalls,omitempty"`
	ToolCall   *llm.ToolCall  `json:"toolCall,omitempty"`
	ToolResult *ToolResult    `json:"toolResult,omitempty"`
	Usage      *llm.Usage     `json:"usage,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	// Approval 是审批事件载荷（approval/asked 与 approval/decided）。
	Approval *Approval `json:"approval,omitempty"`
	// Compaction 是压缩事件载荷（见 EventCompaction 与 Compaction 类型）。
	Compaction *Compaction `json:"compaction,omitempty"`
	// Error 是 turn/end 的失败详情：分类之外的诊断信息（出错原因、终止理由）。
	//
	// 只进日志、不进模型投影（project() 不投影 turn/end）——日志是审计凭据，
	// 失败若只有分类，事后只剩"失败了"三个字，无从追查是模型不可用、证书
	// 过期还是守卫拦的。完整错误仍由返回值交给宿主，二者不互相替代。
	Error string `json:"error,omitempty"`
}

// MaxErrorDetail 是记入日志的错误详情长度上限（超出部分截断）。
//
// 错误串可能很长（服务端返回片段、嵌套的 wrap 链），日志不该被它撑爆；
// 完整内容仍在返回值里，宿主自行决定怎么呈现。
const MaxErrorDetail = 4096

// ErrorDetail 把错误转成可入日志的详情：nil 返回空串，超长按 UTF-8 边界截断。
func ErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) <= MaxErrorDetail {
		return s
	}
	cut := MaxErrorDetail
	// 回退到 rune 边界：切在续字节中间会切出半个字符。
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…（已截断）"
}

// Session 是一个会话：append-only 事件日志 + 投影。
// 并发契约：一个 Session 同一时刻只允许一个 Run（docs/sdk-contracts.md）；
// Append 内部加锁仅防误用，不承载多写者语义。
type Session struct {
	ID string

	mu      sync.Mutex
	seq     int64
	events  []Event
	persist Persistence
}

// New 创建空会话。persist 可为 nil（纯内存，行为不变）。
func New(id string, persist Persistence) *Session {
	return &Session{ID: id, persist: persist}
}

// Append 追加一条事件：填充 Seq 与 Time 后入日志。
// fail-fast：持久化写入失败时事件不进内存，返回错误（保「日志即事实」）。
func (s *Session) Append(ev Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ev.Seq = s.seq + 1
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if s.persist != nil {
		if err := s.persist.Append(s.ID, ev); err != nil {
			return Event{}, err
		}
	}
	s.seq = ev.Seq
	s.events = append(s.events, ev)
	return ev, nil
}

// Events 返回事件日志的副本（调用方修改副本不影响日志）。
func (s *Session) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// Messages 将事件日志投影为对话消息（派生而非存储，可随时重算）：
// user/assistant 事件原样成为消息；tool/result 成为 tool 消息；
// tool/call 仅审计留痕，不投影（assistant 消息已携带 ToolCalls）。
func (s *Session) Messages() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return project(s.events)
}

// SeqMessage 是投影中的一条消息及其来源事件序号：压缩切割需要知道“从
// 哪条事件起保留”，而 Messages 的返回值不带序号——给切割器用的视图带。
type SeqMessage struct {
	Seq     int64
	Message llm.Message
}

// View 返回当前投影视图（消息 + 事件序号）：压缩器用它定切割点，
// 切割下标处的 Seq 就是压缩事件的 FirstKeptSeq。
func (s *Session) View() []SeqMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return projectSeq(s.events)
}

// ContextTokens 估算当前投影视图的上下文大小（压缩触发用）。
//
// 对齐 PI 的两段式：**基座是真实值**——最后一次模型调用的返回用量
// （totalTokens，或分项之和：新鲜输入+缓存读+缓存写+输出，PI 的
// calculateContextTokens 同款）；它之后的新增事件还没有权威数字（要等
// 下一次调用返回才知道），按 chars/4 估算。完全没有用量记录时
// （网关不给 usage）全量估算。这是量级正确的启发式，不是分词；
// 对中文偏低估（触发偏晚），确切性依赖 llm 层未来的分词器。
func (s *Session) ContextTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	base, baseIdx := 0, -1
	for i := len(s.events) - 1; i >= 0; i-- {
		u := s.events[i].Usage
		if u == nil {
			continue
		}
		// PI 的 calculateContextTokens：原生 total 优先，分项和兑底
		//（MoS 口径：新鲜输入 + 缓存读 + 缓存写 + 输出 = 上下文里的全部）。
		if u.TotalTokens > 0 {
			base = u.TotalTokens
		} else {
			base = u.FreshInputTokens + u.Details.Input.CacheRead + u.Details.Input.CacheWrite + u.CompletionTokens
		}
		baseIdx = i
		break
	}
	if baseIdx < 0 {
		base = 0
		baseIdx = -1
	}
	for _, ev := range s.events[baseIdx+1:] {
		base += estimateEventTokens(&ev)
	}
	return base
}

// estimateEventTokens 估算单条事件进入上下文的 token 量（chars/4）。
// tool/call 审计事件不计：它不进投影，工具调用已随 assistant 事件的
// ToolCalls 计过——重复计会让触发偏早。
func estimateEventTokens(ev *Event) int {
	n := len(ev.Text) + len(ev.Reasoning)
	if ev.ToolResult != nil {
		n += len(ev.ToolResult.Content)
	}
	for _, c := range ev.ToolCalls {
		n += len(c.Name) + len(c.Args)
	}
	if ev.Compaction != nil {
		n += len(ev.Compaction.Summary)
	}
	return (n + 3) / 4
}

// NextTurn 返回下一轮的轮次序号（派生：日志中已出现的最大 Turn + 1）。
// 驱动用它开新一轮，无需自维护计数器（Restore 后自然接续）。
func (s *Session) NextTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.events {
		if ev.Turn > n {
			n = ev.Turn
		}
	}
	return n + 1
}

// TotalUsage 折叠日志中所有 assistant 事件的模型调用用量（派生值，不存储：
// Restore 重放后自动重建，Persistence 无需感知聚合逻辑）。
// 仅累计三个主计数；Usage.Details 明细不折叠。
// TotalUsage 汇总整个会话的 token 用量。
//
// 三个主计数直接相加；**明细也累计**（缓存读/写、推理 token）——不然缓存命
// 掩在“输入”里看不出来（Anthropic 的输入本就不含缓存，OpenAI 的含）。
// FreshInputTokens 取各项之和（去缓存口径，跨协议可比）。
func (s *Session) TotalUsage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total llm.Usage
	for _, ev := range s.events {
		if ev.Usage == nil {
			continue
		}
		// assistant 与 compaction 事件都携带用量：摘要调用的消耗同样计入
		// 会话总量（PI："session totals include summarization work"）。
		total.PromptTokens += ev.Usage.PromptTokens
		total.FreshInputTokens += ev.Usage.FreshInputTokens
		total.CompletionTokens += ev.Usage.CompletionTokens
		total.TotalTokens += ev.Usage.TotalTokens
		total.Details.Input.CacheRead += ev.Usage.Details.Input.CacheRead
		total.Details.Input.CacheWrite += ev.Usage.Details.Input.CacheWrite
		total.Details.Input.Audio += ev.Usage.Details.Input.Audio
		total.Details.Output.Reasoning += ev.Usage.Details.Output.Reasoning
		total.Details.Output.Audio += ev.Usage.Details.Output.Audio
	}
	return total
}

// project 把事件序列投影为对话消息（带序号版 projectSeq 的去序包装）。
func project(events []Event) []llm.Message {
	seqs := projectSeq(events)
	out := make([]llm.Message, 0, len(seqs))
	for _, sm := range seqs {
		out = append(out, sm.Message)
	}
	return out
}

// projectSeq 把事件序列投影为带序号的对话消息：turn/* 与 tool/call 不投影
// （审计专用），user/assistant/tool-result 按序映射；compaction 执行遮蔽。
//
// 投影合法性自愈：assistant 声明的工具调用若在日志里找不到配对的
// tool/result（进程在执行期间被硬杀的残留——result 永远没落盘），投影时
// 补一条合成失败回执，保证「assistant 的 ToolCalls 与随后的 tool/result
// 按 CallID 成对」对外永远成立，否则服务商直接拒收历史。日志本身不动：
// 留洞就是发生过的事实（append-only）， healed 的是派生态。
// 合成回执按声明序补在首个非 tool/result 事件之前（协议要求 tool 消息
// 紧跟在带调用的 assistant 消息之后），序号继承声明它的 assistant 事件——
// 压缩遮蔽时它与调用方同生共死，成对不变式不被遮蔽破坏。
func projectSeq(events []Event) []SeqMessage {
	out := make([]SeqMessage, 0, len(events))
	// pending 是已投影、尚未见到回执的调用（按声明序）；answered 去重已回填的 CallID。
	var pending []llm.ToolCall
	answered := map[string]bool{}
	lastAssistantSeq := int64(0)
	// flush 把仍无回执的调用补上合成失败结果，清空本批配对状态。
	// 触发点：遇到任何非 tool/result 事件、以及日志结尾。
	flush := func() {
		for _, c := range pending {
			if !answered[c.ID] {
				out = append(out, SeqMessage{Seq: lastAssistantSeq, Message: llm.Message{
					Role:       llm.RoleTool,
					Content:    fmt.Sprintf("（%s 调用在执行期间被中断，没有结果；如仍需要请重新调用）", c.Name),
					ToolCallID: c.ID,
				}})
			}
		}
		pending = pending[:0]
		answered = make(map[string]bool)
	}
	for i := range events {
		ev := &events[i]
		switch ev.Kind {
		case EventUser:
			flush()
			out = append(out, SeqMessage{Seq: ev.Seq, Message: llm.Message{Role: llm.RoleUser, Content: ev.Text}})
		case EventAssistant:
			flush()
			lastAssistantSeq = ev.Seq
			out = append(out, SeqMessage{Seq: ev.Seq, Message: llm.Message{
				Role:             llm.RoleAssistant,
				Content:          ev.Text,
				ReasoningContent: ev.Reasoning, // 思维链回传：thinking 模式多轮必需
				ToolCalls:        ev.ToolCalls,
			}})
			pending = append(pending, ev.ToolCalls...)
		case EventToolResult:
			if r := ev.ToolResult; r != nil {
				out = append(out, SeqMessage{Seq: ev.Seq, Message: llm.Message{Role: llm.RoleTool, Content: r.Content, ToolCallID: r.CallID}})
				answered[r.CallID] = true
			}
		case EventCompaction:
			if ev.Compaction == nil || ev.Compaction.Summary == "" {
				continue
			}
			flush()
			// 遮蔽：丢掉 FirstKeptSeq 之前的消息，注入摘要（对齐 PI 的
			// rebuild——LLM 看到 [system, summary, kept...]，摘要在前）；旧摘要
			// 若也在被遮蔽之列就一并消失：新摘要在生成时已把旧摘要折叠进去
			//（迭代压缩走 previous-summary 提示词，PI 同款）。
			kept := out[:0:0]
			for _, sm := range out {
				if sm.Seq >= ev.Compaction.FirstKeptSeq {
					kept = append(kept, sm)
				}
			}
			summary := SeqMessage{Seq: ev.Seq, Message: llm.Message{
				Role:    llm.RoleUser,
				Content: CompactionSummaryPrefix + "\n" + ev.Compaction.Summary,
			}}
			out = append(append([]SeqMessage{summary}, kept...))
		}
	}
	flush() // 日志尾部的孤儿调用（进程中断残留）
	return out
}

// EstimateTokens 用 chars/4 启发式估算文本的 token 数（PI 同款：保守、
// 高估；对中文偏低，但压缩触发与切割只需要量级正确的估算，不需要分词器）。
func EstimateTokens(s string) int {
	return (len(s) + 3) / 4
}
