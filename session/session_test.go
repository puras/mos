package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos/llm"
)

func mustAppend(t *testing.T, s *Session, ev Event) Event {
	t.Helper()
	out, err := s.Append(ev)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return out
}

func TestAppendSeqContinuityAndCopy(t *testing.T) {
	s := New("t1", nil)
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Text: "你好"})
	ev := mustAppend(t, s, Event{Kind: EventAssistant, Text: "你好，需要什么帮助？"})

	if ev.Seq != 3 {
		t.Fatalf("seq = %d, want 3（全局递增从 1 开始）", ev.Seq)
	}
	if ev.Time.IsZero() {
		t.Fatal("Append 应填充 Time")
	}

	// Events 返回副本：改动副本不影响日志
	got := s.Events()
	got[0].Kind = "hacked"
	if s.Events()[0].Kind == "hacked" {
		t.Fatal("Events 必须返回副本，调用方不得改动日志")
	}
}

func TestProjection(t *testing.T) {
	s := New("t2", nil)
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Text: "看下文件"})
	mustAppend(t, s, Event{Kind: EventAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"a.txt"}`}}})
	// tool/call 仅审计留痕，不投影
	mustAppend(t, s, Event{Kind: EventToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "read_file"}})
	mustAppend(t, s, Event{Kind: EventToolResult, ToolResult: &ToolResult{CallID: "c1", Content: "文件内容"}})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "内容是……"})
	mustAppend(t, s, Event{Kind: EventTurnEnd, Turn: 1, Reason: "completed"})

	msgs := s.Messages()
	want := []struct {
		role    string
		content string
	}{
		{"user", "看下文件"},
		{"assistant", ""}, // 携带 ToolCalls
		{"tool", "文件内容"},
		{"assistant", "内容是……"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("投影 %d 条消息, want %d（turn/* 与 tool/call 不投影）", len(msgs), len(want))
	}
	for i, w := range want {
		if string(msgs[i].Role) != w.role || msgs[i].Content != w.content {
			t.Fatalf("msgs[%d] = %s/%q, want %s/%q", i, msgs[i].Role, msgs[i].Content, w.role, w.content)
		}
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "read_file" {
		t.Fatalf("assistant 消息应携带 ToolCalls: %+v", msgs[1])
	}
	if msgs[2].ToolCallID != "c1" {
		t.Fatalf("tool 消息应携带 ToolCallID: %+v", msgs[2])
	}
}

func TestJSONLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	jl, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	id := "rt"
	if err := jl.SaveHeader(id, Header{ID: id}); err != nil {
		t.Fatalf("SaveHeader: %v", err)
	}

	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Text: "你好"})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "嗨", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}})
	mustAppend(t, s, Event{Kind: EventTurnEnd, Turn: 1, Reason: "completed"})

	events, err := jl.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(events) != 4 || events[0].Kind != EventTurnStart || events[2].Text != "嗨" {
		t.Fatalf("round-trip 不一致: %+v", events)
	}
	if events[0].Seq != 1 || events[3].Seq != 4 {
		t.Fatalf("Seq 未持久化: %d..%d", events[0].Seq, events[3].Seq)
	}

	// Restore 重建：日志与投影一致，且可继续追加
	rs, err := Restore(id, jl)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(rs.Messages()) != 2 {
		t.Fatalf("恢复后投影 = %d 条, want 2", len(rs.Messages()))
	}
	if got := rs.TotalUsage(); got.TotalTokens != 15 {
		t.Fatalf("恢复后 TotalUsage = %+v, want TotalTokens 15（用量随事件重放重建）", got)
	}
	mustAppend(t, rs, Event{Kind: EventUser, Text: "继续"})
	events, _ = jl.Load(id)
	if len(events) != 5 || events[4].Seq != 5 {
		t.Fatalf("恢复后续写应持久化且 Seq 续接: %+v", events)
	}
}

func TestRestoreRepairsOrphanTurn(t *testing.T) {
	dir := t.TempDir()
	jl, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	id := "orphan"
	jl.SaveHeader(id, Header{ID: id})
	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Text: "任务"})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "干一半"}) // 崩溃：无 turn/end

	rs, err := Restore(id, jl)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	events := rs.Events()
	last := events[len(events)-1]
	if last.Kind != EventTurnEnd || last.Reason != "interrupted" {
		t.Fatalf("孤儿 turn 应合成 interrupted 收尾, got %s/%s", last.Kind, last.Reason)
	}
	if last.Seq != 4 {
		t.Fatalf("合成事件 Seq = %d, want 4（内存续接盘上 3 条，不回写）", last.Seq)
	}
	// 不回写已有日志：后端里仍只有 3 条原始事件
	disk, _ := jl.Load(id)
	if len(disk) != 3 {
		t.Fatalf("合成事件不得回写后端: 磁盘 %d 条", len(disk))
	}

	// 完整日志不应误报孤儿
	s2 := New("ok", jl)
	mustAppend(t, s2, Event{Kind: EventTurnStart, Turn: 2})
	mustAppend(t, s2, Event{Kind: EventTurnEnd, Turn: 2, Reason: "completed"})
	if s2.repairOrphanTurn() {
		t.Fatal("尾部完整的日志不应报孤儿 turn")
	}
}

func TestAppendFailFast(t *testing.T) {
	boom := errors.New("disk full")
	s := New("ff", failPersist{err: boom})
	if _, err := s.Append(Event{Kind: EventUser, Text: "x"}); !errors.Is(err, boom) {
		t.Fatalf("写失败应返回错误, got %v", err)
	}
	// fail-fast：写失败的事件不得进内存
	if got := s.Events(); len(got) != 0 {
		t.Fatalf("写失败后日志应为空, got %d 条", len(got))
	}
}

type failPersist struct{ err error }

func (f failPersist) SaveHeader(string, Header) error { return nil }
func (f failPersist) Append(string, Event) error      { return f.err }
func (f failPersist) Load(string) ([]Event, error)    { return nil, nil }
func (f failPersist) Close() error                    { return nil }

func TestJSONLTrailingHalfLine(t *testing.T) {
	dir := t.TempDir()
	jl, _ := NewJSONL(dir)
	id := "half"
	jl.SaveHeader(id, Header{ID: id})
	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventUser, Text: "完整的一条"})

	// 模拟崩溃：追加半行 JSON（无换行结尾）
	p := filepath.Join(dir, id+".jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"seq":2,"kind":"user","te`)
	f.Close()

	events, err := jl.Load(id)
	if err != nil {
		t.Fatalf("尾部半行应被容忍: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("半行应被丢弃, got %d 条", len(events))
	}

	// 中段损坏必须报错，不允许静默跳过
	full, _ := os.ReadFile(p)
	corrupted := strings.Replace(string(full), `"完整的一条"`, `"损坏"`, 1) + "\n" + `{bad json}` + "\n"
	os.WriteFile(p, []byte(corrupted), 0o644)
	if _, err := jl.Load(id); err == nil {
		t.Fatal("中段损坏应返回错误")
	}
}

func TestRestoreMissingSession(t *testing.T) {
	jl, _ := NewJSONL(t.TempDir())
	s, err := Restore("nope", jl)
	if err != nil {
		t.Fatalf("不存在的会话应返回空会话而非错误: %v", err)
	}
	if len(s.Events()) != 0 || len(s.Messages()) != 0 {
		t.Fatal("空会话不应有事件或消息")
	}
}

func TestTotalUsageDerivedNotStored(t *testing.T) {
	dir := t.TempDir()
	jl, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	id := "usage"
	jl.SaveHeader(id, Header{ID: id})
	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventUser, Text: "q1"})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "a1", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}})
	mustAppend(t, s, Event{Kind: EventUser, Text: "q2"})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "a2", Usage: &llm.Usage{PromptTokens: 30, CompletionTokens: 6, TotalTokens: 36}})
	// 无用量的事件不参与折叠
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "a3"})

	got := s.TotalUsage()
	if got.PromptTokens != 40 || got.CompletionTokens != 10 || got.TotalTokens != 50 {
		t.Fatalf("TotalUsage = %+v, want 40/10/50", got)
	}

	// 派生而非存储：Restore 重放后自动重建，后端不感知聚合
	rs, err := Restore(id, jl)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rs.TotalUsage() != (llm.Usage{PromptTokens: 40, CompletionTokens: 10, TotalTokens: 50}) {
		t.Fatalf("恢复后 TotalUsage = %+v", rs.TotalUsage())
	}
}
