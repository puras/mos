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
//	approval/asked   问了人工（审批请求发出；不投影，仅审计）
//	approval/decided 人工答了（与 asked 用同一个 id 配对；不投影，仅审计）
//
// 最后两种是 PRD 点名的例外（§可观测「approval/* 等事件全量落会话日志」、
// G4「审批挂起-恢复 E2E」）：审批的审计链断在这里就不知道谁批的。
package session

import (
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
func (s *Session) TotalUsage() llm.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total llm.Usage
	for _, ev := range s.events {
		if ev.Usage != nil {
			total.PromptTokens += ev.Usage.PromptTokens
			total.CompletionTokens += ev.Usage.CompletionTokens
			total.TotalTokens += ev.Usage.TotalTokens
		}
	}
	return total
}

func project(events []Event) []llm.Message {
	out := make([]llm.Message, 0, len(events))
	for _, ev := range events {
		switch ev.Kind {
		case EventUser:
			out = append(out, llm.Message{Role: llm.RoleUser, Content: ev.Text})
		case EventAssistant:
			out = append(out, llm.Message{
				Role:             llm.RoleAssistant,
				Content:          ev.Text,
				ReasoningContent: ev.Reasoning, // 思维链回传：thinking 模式多轮必需
				ToolCalls:        ev.ToolCalls,
			})
		case EventToolResult:
			if r := ev.ToolResult; r != nil {
				out = append(out, llm.Message{Role: llm.RoleTool, Content: r.Content, ToolCallID: r.CallID})
			}
		}
	}
	return out
}
