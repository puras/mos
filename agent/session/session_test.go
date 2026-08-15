package session

import (
	"encoding/json"
	"reflect"
	"testing"

	"hegq.net/mos/agent/event"
	"hegq.net/mos/llm"
)

// TestSessionSaveRestoreRoundTrip 覆盖事件日志与任务状态的序列化往返：
// 消息事件（user/assistant/tool）、压缩事件、会话元数据与 TaskState。
func TestSessionSaveRestoreRoundTrip(t *testing.T) {
	sess := NewSession("sess-001")
	_ = sess.AppendUserMessage("请计算 1+2 并汇报", "user")
	_ = sess.AppendAssistantMessage(1, "让我计算", "先调用计算器", []llm.ToolCall{
		{ID: "call_1", Name: "read_file", Args: `{"expr":"1+2"}`},
	}, llm.Usage{})
	_ = sess.AppendToolCall(1, &llm.ToolCall{ID: "call_1", Name: "read_file", Args: `{"expr":"1+2"}`})
	_ = sess.AppendToolResult(1, &llm.ToolCall{ID: "call_1", Name: "read_file"}, 3, "3", nil)
	_ = sess.AppendAssistantMessage(2, "结果是 3", "推理：1+2=3", nil, llm.Usage{PromptTokens: 10})
	_ = sess.Append(event.SessionEventCompactionSummary, event.CompactionSummaryData{Summary: "已确认计算结果为 3", ShadowedFrom: 2, ShadowedTo: 3})
	sess.Header.CWD = "/tmp/work"
	sess.State = TaskState{
		Goal:    "请计算 1+2 并汇报",
		Facts:   []Fact{{Summary: "1+2=3", Evidence: "3", Tool: "read_file"}},
		Pending: []string{"汇报结果"},
		Status:  StatusRunning,
	}

	data, err := sess.Save()
	if err != nil {
		t.Fatalf("Save error: %v", err)
	}
	got, err := RestoreSession(data)
	if err != nil {
		t.Fatalf("RestoreSession error: %v", err)
	}
	if got.ID != sess.ID {
		t.Errorf("ID mismatch: got %q want %q", got.ID, sess.ID)
	}
	if !reflect.DeepEqual(got.Events, sess.Events) {
		t.Errorf("Events mismatch after round-trip:\ngot  %+v\nwant %+v", got.Events, sess.Events)
	}
	if !reflect.DeepEqual(got.State, sess.State) {
		t.Errorf("State mismatch after round-trip:\ngot  %+v\nwant %+v", got.State, sess.State)
	}
	if got.Header.CWD != "/tmp/work" || got.Header.Version != sessionFormatVersion {
		t.Errorf("Header mismatch: %+v", got.Header)
	}
	// 事件日志往返后消息投影一致
	if !reflect.DeepEqual(got.Messages(), sess.Messages()) {
		t.Errorf("Messages projection mismatch after round-trip")
	}
}

// TestSessionSaveRestore_EmptySession 空会话（仅 ID）也应可往返。
func TestSessionSaveRestore_EmptySession(t *testing.T) {
	sess := NewSession("empty-001")
	data, err := sess.Save()
	if err != nil {
		t.Fatalf("Save error: %v", err)
	}
	got, err := RestoreSession(data)
	if err != nil {
		t.Fatalf("RestoreSession error: %v", err)
	}
	if got.ID != "empty-001" || len(got.Events) != 0 {
		t.Errorf("unexpected restored session: %+v", got)
	}
}

// TestRestoreSession_Invalid 非法 JSON 应返回错误而非 panic。
func TestRestoreSession_Invalid(t *testing.T) {
	if _, err := RestoreSession([]byte("not-json")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if _, err := RestoreSession(nil); err == nil {
		t.Fatal("expected error for nil data")
	}
}

// TestSessionEventSeqContinuous Seq 必须从 1 开始连续递增（不变式）。
func TestSessionEventSeqContinuous(t *testing.T) {
	sess := NewSession("s1")
	_ = sess.AppendUserMessage("hi", "user")
	_ = sess.AppendAssistantMessage(1, "hello", "", nil, llm.Usage{})
	if len(sess.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(sess.Events))
	}
	for i, ev := range sess.Events {
		if want := int64(i + 1); ev.Seq != want {
			t.Fatalf("events[%d].Seq = %d, want %d", i, ev.Seq, want)
		}
	}
}

// TestDeriveMessages 投影规则：消息事件重建、chunk 聚合、摘要替换语义。
func TestDeriveMessages(t *testing.T) {
	sess := NewSession("s1")
	_ = sess.AppendUserMessage("用户输入", "user")
	_ = sess.AppendAssistantMessage(1, "回答一", "思考一", nil, llm.Usage{})
	_ = sess.Append(event.SessionEventAssistantChunk, event.AssistantChunkData{Turn: 2, Delta: "部"})
	_ = sess.Append(event.SessionEventAssistantChunk, event.AssistantChunkData{Turn: 2, Delta: "分"})
	_ = sess.AppendAssistantMessage(2, "调用工具", "思考二", []llm.ToolCall{{ID: "c1", Name: "read_file", Args: `{"a":1}`}}, llm.Usage{})
	_ = sess.AppendToolResult(2, &llm.ToolCall{ID: "c1", Name: "read_file"}, 1, "1", nil)

	msgs := sess.Messages()
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].Content != "用户输入" {
		t.Fatalf("msgs[0] = %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].ReasoningContent != "思考一" {
		t.Fatalf("msgs[1] = %+v", msgs[1])
	}
	if msgs[2].Role != llm.RoleAssistant || len(msgs[2].ToolCalls) != 1 || msgs[2].ReasoningContent != "思考二" {
		t.Fatalf("msgs[2] = %+v", msgs[2])
	}
	if msgs[3].Role != llm.RoleTool || msgs[3].Content != "1" {
		t.Fatalf("msgs[3] = %+v", msgs[3])
	}
}

// TestDeriveMessagesChunkOnly 仅 chunk 无完整消息（异常中断）时聚合为一条消息。
func TestDeriveMessagesChunkOnly(t *testing.T) {
	sess := NewSession("s1")
	_ = sess.Append(event.SessionEventAssistantChunk, event.AssistantChunkData{Turn: 1, Delta: "你"})
	_ = sess.Append(event.SessionEventAssistantChunk, event.AssistantChunkData{Turn: 1, Delta: "好"})
	msgs := sess.Messages()
	if len(msgs) != 1 || msgs[0].Content != "你好" {
		t.Fatalf("chunk aggregation failed: %+v", msgs)
	}
}

// TestDeriveMessagesCompactionSummary 压缩摘要替换阴影范围内的旧消息。
func TestDeriveMessagesCompactionSummary(t *testing.T) {
	sess := NewSession("s1")
	_ = sess.AppendUserMessage("问题一", "user")
	_ = sess.AppendAssistantMessage(1, "回答一", "", nil, llm.Usage{})
	_ = sess.AppendUserMessage("问题二", "user")
	_ = sess.AppendAssistantMessage(2, "回答二", "", nil, llm.Usage{})
	// 压缩：摘要替换前 2 条消息事件（seq 1-2）
	_ = sess.Append(event.SessionEventCompactionSummary, event.CompactionSummaryData{
		Summary: "历史摘要", ShadowedFrom: 1, ShadowedTo: 2,
	})

	msgs := sess.Messages()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (summary + 2 kept): %+v", len(msgs), msgs)
	}
	if msgs[0].Summary != true || msgs[0].Content != "历史摘要" {
		t.Fatalf("msgs[0] = %+v, want summary message", msgs[0])
	}
	if msgs[1].Content != "问题二" || msgs[2].Content != "回答二" {
		t.Fatalf("kept messages wrong: %+v", msgs)
	}
}

// TestTaskStatusJSON 状态序列化为可读字符串，反序列化兼容字符串与数字。
func TestTaskStatusJSON(t *testing.T) {
	cases := []struct {
		status TaskStatus
		name   string
	}{
		{StatusPending, "pending"},
		{StatusRunning, "running"},
		{StatusBlocked, "blocked"},
		{StatusCompleted, "completed"},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.status)
		if err != nil {
			t.Fatalf("Marshal %s error: %v", c.name, err)
		}
		if string(b) != `"`+c.name+`"` {
			t.Errorf("Marshal %d = %s, want %q", c.status, b, `"`+c.name+`"`)
		}
		var got TaskStatus
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal %s error: %v", c.name, err)
		}
		if got != c.status {
			t.Errorf("Unmarshal %s = %d, want %d", c.name, got, c.status)
		}
	}

	// 数字形态兼容
	var n TaskStatus
	if err := json.Unmarshal([]byte("3"), &n); err != nil {
		t.Fatalf("Unmarshal numeric status error: %v", err)
	}
	if n != StatusCompleted {
		t.Errorf("numeric status = %d, want %d", n, StatusCompleted)
	}

	// 未知字符串报错
	var bad TaskStatus
	if err := json.Unmarshal([]byte(`"unknown"`), &bad); err == nil {
		t.Fatal("expected error for unknown status string")
	}
}
