package agent

import (
	"encoding/json"
	"reflect"
	"testing"

	"hegq.net/mos/llm"
)

// TestSessionSaveRestoreRoundTrip 覆盖全部消息类型与任务状态的序列化往返：
// system / user / assistant（含工具调用与推理内容）/ tool / summary / custom，
// 以及运行期元数据（ExcludeFromContext）的保留。
func TestSessionSaveRestoreRoundTrip(t *testing.T) {
	excluded := UserMsg("不进上下文的内部消息")
	excluded.ExcludeFromContext = true

	sess := &Session{
		ID: "sess-001",
		History: []InternalMessage{
			SystemMsg("你是助手"),
			UserMsg("请计算 1+2 并汇报"),
			AssistantToolMsg("让我计算", "先调用计算器", []llm.ToolCall{
				{ID: "call_1", Name: "calculator", Args: `{"expr":"1+2"}`},
			}),
			ToolMsg("call_1", "calculator", "3"),
			AssistantMsg("结果是 3", "推理：1+2=3"),
			SummaryMsg("已确认计算结果为 3"),
			CustomMsg("rag", "[来源: doc#1] (score 0.95): 1+2=3"),
			excluded,
		},
		State: TaskState{
			Goal:    "请计算 1+2 并汇报",
			Facts:   []Fact{{Summary: "1+2=3", Evidence: "3", Tool: "calculator"}},
			Pending: []string{"汇报结果"},
			Status:  StatusRunning,
		},
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
	if !reflect.DeepEqual(got.History, sess.History) {
		t.Errorf("History mismatch after round-trip:\ngot  %+v\nwant %+v", got.History, sess.History)
	}
	if !reflect.DeepEqual(got.State, sess.State) {
		t.Errorf("State mismatch after round-trip:\ngot  %+v\nwant %+v", got.State, sess.State)
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
	if got.ID != "empty-001" || len(got.History) != 0 {
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
