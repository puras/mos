package agent

import (
	"encoding/json"
	"fmt"

	"hegq.net/mos/agent/event"
	"hegq.net/mos/llm"
)

// sessionFormatVersion 是会话日志格式版本（SessionHeader.Version）。
const sessionFormatVersion = 2

// SessionHeader 是会话存储元数据，与事件日志分离、不进模型面：
// version（日志格式版本）/ cwd（工作目录）/ lineage（fork 父会话）/ seed 边界。
// 对应架构 v2 §3.2（对齐 dsh SessionHeader）。
type SessionHeader struct {
	Version  int    `json:"version"`
	CWD      string `json:"cwd,omitempty"`
	ParentID string `json:"parent_id,omitempty"` // fork lineage：父会话 ID（SubAgent 关联）
	SeedSeq  int64  `json:"seed_seq,omitempty"`  // seed 边界：fork 时复制的日志序号
}

// SessionEvent 是一条不可变会话事件：Seq 连续（1 起），Type + Data（json）。
// 持久化/回放/恢复的最小单元（架构 v2 §3.2 / §4.1）。
type SessionEvent struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Session 是智能体会话：append-only 事件日志是唯一事实来源，
// 消息历史经 deriveMessages 投影派生（架构 v2 §3.2）。
type Session struct {
	ID     string         `json:"id"`
	Events []SessionEvent `json:"events"`
	State  TaskState      `json:"state"`
	Header SessionHeader  `json:"header,omitempty"`
}

// NewSession 创建指定 ID 的空会话（版本号自动填充）。
func NewSession(id string) *Session {
	return &Session{ID: id, Header: SessionHeader{Version: sessionFormatVersion}}
}

// Append 追加一条会话事件，自动分配连续 Seq。
// 事件日志是唯一事实来源：任何进入模型请求的消息必须能从 Events 重建（不变式）。
func (s *Session) Append(typ string, data any) error {
	if s == nil {
		return fmt.Errorf("agent: append event to nil session")
	}
	ev := SessionEvent{Seq: int64(len(s.Events)) + 1, Type: typ, Data: event.MarshalData(data)}
	s.Events = append(s.Events, ev)
	return nil
}

// Messages 从事件日志投影出模型消息历史（每次请求前调用，见 deriveMessages）。
func (s *Session) Messages() []InternalMessage {
	if s == nil {
		return nil
	}
	return deriveMessages(s.Events)
}

// deriveMessages 从事件日志投影出模型消息历史。
// 规则（对齐 dsh "Model-visible means logged"）：
//   - user/message → 用户消息（保留 Source 标注）
//   - assistant/message → 助手消息（含工具调用时保留 ToolCalls 与推理内容）
//   - assistant/chunk → 流式增量累积；同轮存在 assistant/message 时被其消费，
//     仅当一轮以 chunk 结尾（异常中断）时聚合为消息
//   - tool/result → 工具结果消息（优先采用 Text，否则重新格式化 Result）
//   - compaction/summary → 历史摘要消息，替换其阴影范围内的旧消息（可回放）
func deriveMessages(events []SessionEvent) []InternalMessage {
	// 预扫描阴影范围与摘要插入位置：
	// compaction/summary 声明被替换的日志序号区间（ShadowedFrom..ShadowedTo），
	// 摘要消息插入到 ShadowedFrom 处（替换语义，先摘要后保留消息）。
	shadowed := make(map[int64]bool)
	var summaries []messageAt
	for _, ev := range events {
		if ev.Type != event.SessionEventCompactionSummary {
			continue
		}
		var d event.CompactionSummaryData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			continue
		}
		for seq := d.ShadowedFrom; seq <= d.ShadowedTo; seq++ {
			shadowed[seq] = true
		}
		summaries = append(summaries, messageAt{pos: d.ShadowedFrom, msg: SummaryMsg(d.Summary)})
	}

	var out []messageAt
	pendingChunk := ""
	chunkPos := int64(0)
	flushChunk := func() {
		if pendingChunk != "" {
			out = append(out, messageAt{pos: chunkPos, msg: AssistantMsg(pendingChunk, "")})
			pendingChunk = ""
		}
	}
	for _, ev := range events {
		if ev.Seq > 0 && shadowed[ev.Seq] {
			continue // 已被压缩摘要替换的旧消息
		}
		switch ev.Type {
		case event.SessionEventUserMessage:
			flushChunk()
			var d event.UserMessageData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				continue
			}
			msg := UserMsg(d.Message)
			if d.Source != "" && d.Source != "user" {
				msg.Source = d.Source
			}
			out = append(out, messageAt{pos: ev.Seq, msg: msg})
		case event.SessionEventAssistantChunk:
			var d event.AssistantChunkData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				continue
			}
			if pendingChunk == "" {
				chunkPos = ev.Seq
			}
			pendingChunk += d.Delta
		case event.SessionEventAssistantMessage:
			pendingChunk = "" // 完整消息消费掉前置 chunk 累积
			var d event.AssistantMessageData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				continue
			}
			if len(d.ToolCalls) > 0 {
				out = append(out, messageAt{pos: ev.Seq, msg: AssistantToolMsg(d.Content, d.Reasoning, d.ToolCalls)})
			} else {
				out = append(out, messageAt{pos: ev.Seq, msg: AssistantMsg(d.Content, d.Reasoning)})
			}
		case event.SessionEventToolResult:
			flushChunk()
			var d event.ToolResultData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				continue
			}
			content := d.Text
			if content == "" {
				content = formatToolResult(d.Result)
			}
			if d.Error != "" {
				content = "工具执行出错: " + d.Error
			}
			out = append(out, messageAt{pos: ev.Seq, msg: ToolMsg(d.CallID, d.Name, content)})
		}
	}
	flushChunk()

	// 合并摘要（按 ShadowedFrom 位置插入）并按位置稳定排序
	out = append(out, summaries...)
	stableSortByPos(out)

	msgs := make([]InternalMessage, 0, len(out))
	for _, p := range out {
		msgs = append(msgs, p.msg)
	}
	return msgs
}

// messageAt 是带日志位置的消息（用于压缩摘要替换排序）。
type messageAt struct {
	pos int64
	msg InternalMessage
}

// stableSortByPos 按位置稳定排序（插入排序，规模小）。
func stableSortByPos(items []messageAt) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].pos < items[j-1].pos; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// messageEventSeqs 返回日志中产生模型面消息的事件序号（按顺序），
// 用于压缩时计算被替换（shadowed）的日志范围。
// 消息事件：user/message、assistant/message、tool/result、compaction/summary。
func messageEventSeqs(events []SessionEvent) []int64 {
	var seqs []int64
	for _, ev := range events {
		switch ev.Type {
		case event.SessionEventUserMessage, event.SessionEventAssistantMessage,
			event.SessionEventToolResult, event.SessionEventCompactionSummary:
			seqs = append(seqs, ev.Seq)
		}
	}
	return seqs
}

// sessionSnapshot 是 Session 的稳定序列化结构（平台持久化契约，
// 见 docs/sdk-contracts.md §3）。字段契约变更须同步文档。
type sessionSnapshot struct {
	ID     string         `json:"id"`
	Events []SessionEvent `json:"events"`
	State  TaskState      `json:"state"`
	Header SessionHeader  `json:"header,omitempty"`
}

// Save 将会话（ID + 事件日志 + 任务状态 + 元数据）序列化为 JSON，供平台持久化。
// 注意：只序列化数据；Agent 配置（模型对象 / 工具集 / 策略）由平台自行持有，
// 恢复会话后需重建 Agent 再 Run 续跑。
func (s *Session) Save() ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("agent: save nil session")
	}
	snap := sessionSnapshot{ID: s.ID, Events: s.Events, State: s.State, Header: s.Header}
	return json.Marshal(snap)
}

// RestoreSession 从 Save 产出的 JSON 恢复会话，用于跨进程 / 重启后的断点续跑。
// 恢复后的会话可直接传给 Driver.Run 继续执行。
func RestoreSession(data []byte) (*Session, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("agent: restore session: empty data")
	}
	var snap sessionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("agent: restore session: %w", err)
	}
	if snap.ID == "" {
		return nil, fmt.Errorf("agent: restore session: missing id")
	}
	sess := &Session{ID: snap.ID, Events: snap.Events, State: snap.State, Header: snap.Header}
	if sess.Header.Version == 0 {
		sess.Header.Version = sessionFormatVersion
	}
	return sess, nil
}

// ---- 事件构造 helper（typed，供驱动与测试使用）----

func (s *Session) appendUserMessage(message, source string) error {
	return s.Append(event.SessionEventUserMessage, event.UserMessageData{Message: message, Source: source})
}

func (s *Session) appendAssistantMessage(turn int, content, reasoning string, calls []llm.ToolCall, usage llm.Usage) error {
	return s.Append(event.SessionEventAssistantMessage, event.AssistantMessageData{
		Turn: turn, Content: content, Reasoning: reasoning, ToolCalls: calls, Usage: usage,
	})
}

func (s *Session) appendToolCall(turn int, call *llm.ToolCall) error {
	if call == nil {
		return nil
	}
	return s.Append(event.SessionEventToolCall, event.ToolCallData{
		Turn: turn, CallID: call.ID, Name: call.Name, Args: call.Args,
	})
}

func (s *Session) appendToolResult(turn int, call *llm.ToolCall, result any, text string, err error) error {
	if call == nil {
		return nil
	}
	d := event.ToolResultData{Turn: turn, CallID: call.ID, Name: call.Name, Result: result, Text: text}
	if err != nil {
		d.Error = err.Error()
	}
	return s.Append(event.SessionEventToolResult, d)
}

// formatToolResult 将工具返回值统一序列化为给模型看的文本。
func formatToolResult(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
