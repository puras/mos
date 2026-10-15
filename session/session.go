// Package session 提供会话事件日志：append-only 事件是唯一事实来源，
// 消息历史由投影派生（不变式「日志即事实」）。
//
// 最小事件目录（预算制：新增一种事件前先删一种）：
//
//	turn/start  一轮开始
//	turn/end    一轮结束（reason: completed / blocked / interrupted / error）
//	user        用户输入（投影为 user 消息）
//	assistant   模型输出（文本 + 可选工具调用；投影为 assistant 消息）
//	tool/call   工具调用留痕（含被守卫拦截的调用；不投影，仅审计）
//	tool/result 工具结果（投影为 tool 消息）
package session

import (
	"sync"
	"time"
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
)

// ToolCall 是一次工具调用（事件与消息共享同一形态，避免翻译边界丢失字段）。
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // JSON 对象文本
}

// ToolResult 是一次工具执行的回填。
type ToolResult struct {
	CallID  string `json:"callId"`
	Content string `json:"content"`
	IsError bool   `json:"isError,omitempty"`
}

// Event 是日志中的一条记录。Seq 由 Session.Append 统一分配，全局递增从 1 开始。
// 载荷字段按 Kind 取用：Text（user/assistant）、ToolCalls（assistant）、
// ToolCall（tool/call）、ToolResult（tool/result）、Reason（turn/end）。
type Event struct {
	Seq        int64       `json:"seq"`
	Kind       EventKind   `json:"kind"`
	Turn       int         `json:"turn,omitempty"`
	Time       time.Time   `json:"time"`
	Text       string      `json:"text,omitempty"`
	ToolCalls  []ToolCall  `json:"toolCalls,omitempty"`
	ToolCall   *ToolCall   `json:"toolCall,omitempty"`
	ToolResult *ToolResult `json:"toolResult,omitempty"`
	Reason     string      `json:"reason,omitempty"`
}

// Message 是投影后的对话消息（协议形，S2 的 llm 层按此转换）。
type Message struct {
	Role       string     `json:"role"` // user / assistant / tool
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`  // 仅 assistant
	ToolCallID string     `json:"toolCallId,omitempty"` // 仅 tool
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
func (s *Session) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return project(s.events)
}

func project(events []Event) []Message {
	out := make([]Message, 0, len(events))
	for _, ev := range events {
		switch ev.Kind {
		case EventUser:
			out = append(out, Message{Role: "user", Content: ev.Text})
		case EventAssistant:
			m := Message{Role: "assistant", Content: ev.Text, ToolCalls: ev.ToolCalls}
			out = append(out, m)
		case EventToolResult:
			if r := ev.ToolResult; r != nil {
				out = append(out, Message{Role: "tool", Content: r.Content, ToolCallID: r.CallID})
			}
		}
	}
	return out
}
