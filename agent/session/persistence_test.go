package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"hegq.net/mos/agent/event"
)

func TestJSONLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	defer j.Close()

	h := SessionHeader{Version: sessionFormatVersion, CWD: "/tmp", ParentID: "p1", SeedSeq: 5}
	if err := j.SaveHeader("s1", h); err != nil {
		t.Fatalf("SaveHeader: %v", err)
	}
	evs := []SessionEvent{
		{Seq: 1, Type: event.SessionEventTurnStart, Data: event.MarshalData(event.TurnData{Turn: 1, Reason: "user"})},
		{Seq: 2, Type: event.SessionEventUserMessage, Data: event.MarshalData(event.UserMessageData{Message: "hi", Source: "user"})},
	}
	for _, ev := range evs {
		if err := j.Append("s1", ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	got, err := j.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Type != event.SessionEventUserMessage {
		t.Fatalf("loaded = %+v", got)
	}
	gotH, err := j.LoadHeader("s1")
	if err != nil {
		t.Fatalf("LoadHeader: %v", err)
	}
	if gotH.CWD != "/tmp" || gotH.ParentID != "p1" || gotH.SeedSeq != 5 {
		t.Fatalf("header = %+v", gotH)
	}
}

func TestJSONLSessionIsolation(t *testing.T) {
	dir := t.TempDir()
	j, _ := NewJSONL(dir)
	defer j.Close()
	_ = j.SaveHeader("a", SessionHeader{Version: 2})
	_ = j.SaveHeader("b", SessionHeader{Version: 2})
	_ = j.Append("a", SessionEvent{Seq: 1, Type: event.SessionEventUserMessage, Data: event.MarshalData(event.UserMessageData{Message: "a"})})
	_ = j.Append("b", SessionEvent{Seq: 1, Type: event.SessionEventUserMessage, Data: event.MarshalData(event.UserMessageData{Message: "b"})})

	evsA, _ := j.Load("a")
	evsB, _ := j.Load("b")
	if len(evsA) != 1 || len(evsB) != 1 || string(evsA[0].Data) == string(evsB[0].Data) {
		t.Fatalf("sessions not isolated: %v %v", evsA, evsB)
	}
}

// TestSessionPersistenceIntegration 验证 Session.Append 同步落盘 + Restore 重放。
func TestSessionPersistenceIntegration(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	defer j.Close()

	sess := NewSession("s1")
	sess.Header.CWD = "/work"
	sess.Persistence = j
	if err := sess.AppendUserMessage("第一轮", "user"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: 1, Reason: "user"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	restored, err := Restore(j, "s1")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.ID != "s1" || restored.Header.CWD != "/work" {
		t.Fatalf("restored = %+v", restored)
	}
	msgs := restored.Messages()
	if len(msgs) != 1 || msgs[0].Content != "第一轮" {
		t.Fatalf("messages = %+v", msgs)
	}
}

// TestRepairInterrupted 验证孤儿 turn/start 合成 interrupted turn/end。
func TestRepairInterrupted(t *testing.T) {
	events := []SessionEvent{
		{Seq: 1, Type: event.SessionEventTurnStart, Data: event.MarshalData(event.TurnData{Turn: 1, Reason: "user"})},
		{Seq: 2, Type: event.SessionEventUserMessage, Data: event.MarshalData(event.UserMessageData{Message: "hi"})},
		{Seq: 3, Type: event.SessionEventTurnEnd, Data: event.MarshalData(event.TurnData{Turn: 1, Reason: "done"})},
		{Seq: 4, Type: event.SessionEventTurnStart, Data: event.MarshalData(event.TurnData{Turn: 2, Reason: "user"})},
		// turn 2 无 turn/end（崩溃）
	}
	repaired := RepairInterrupted(events)
	if len(repaired) != 5 {
		t.Fatalf("repaired = %d, want 5（4 条原始 + 1 条合成）", len(repaired))
	}
	last := repaired[len(repaired)-1]
	if last.Type != event.SessionEventTurnEnd {
		t.Fatalf("last = %s, want turn/end", last.Type)
	}
	var d event.TurnData
	if err := json.Unmarshal(last.Data, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Reason != "interrupted" || d.Turn != 2 {
		t.Fatalf("synthetic turn/end = %+v, want {Turn:2 interrupted}", d)
	}
}

// TestRepairInterruptedNested 验证按 turn 号集合求差（而非栈配对）：
// 嵌套/乱序关闭时孤儿判定仍正确（start(1)→start(2)→end(1) 的孤儿是 turn 2）。
func TestRepairInterruptedNested(t *testing.T) {
	events := []SessionEvent{
		{Seq: 1, Type: event.SessionEventTurnStart, Data: event.MarshalData(event.TurnData{Turn: 1, Reason: "user"})},
		{Seq: 2, Type: event.SessionEventTurnStart, Data: event.MarshalData(event.TurnData{Turn: 2, Reason: "user"})},
		{Seq: 3, Type: event.SessionEventTurnEnd, Data: event.MarshalData(event.TurnData{Turn: 1, Reason: "done"})},
	}
	repaired := RepairInterrupted(events)
	if len(repaired) != 4 {
		t.Fatalf("repaired = %d, want 4", len(repaired))
	}
	var d event.TurnData
	if err := json.Unmarshal(repaired[3].Data, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Turn != 2 || d.Reason != "interrupted" {
		t.Fatalf("synthetic turn/end = %+v, want {Turn:2 interrupted}", d)
	}
}

// TestJSONLTolerateTruncatedLine 验证损坏半行被跳过（崩溃截断写）。
func TestJSONLTolerateTruncatedLine(t *testing.T) {
	dir := t.TempDir()
	j, _ := NewJSONL(dir)
	defer j.Close()
	_ = j.SaveHeader("s1", SessionHeader{Version: 2})
	_ = j.Append("s1", SessionEvent{Seq: 1, Type: event.SessionEventUserMessage, Data: event.MarshalData(event.UserMessageData{Message: "ok"})})
	// 模拟尾部半行（崩溃截断写）
	f, err := os.OpenFile(filepath.Join(dir, "s1.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, _ = f.WriteString(`{"event":{"seq":2,"type":"turn/start","data":{`)
	f.Close()

	evs, err := j.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("evs = %d, want 1（半行被跳过）", len(evs))
	}
}

// TestRestoreEmptySession 验证恢复不存在的会话（空会话，不报错）。
func TestRestoreEmptySession(t *testing.T) {
	j, _ := NewJSONL(t.TempDir())
	defer j.Close()
	sess, err := Restore(j, "missing")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if sess.ID != "missing" || len(sess.Events) != 0 {
		t.Fatalf("sess = %+v", sess)
	}
}

func TestSaveLoadStateRoundTrip(t *testing.T) {
	j, _ := NewJSONL(t.TempDir())
	defer j.Close()

	if _, err := j.LoadState("s"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load missing state err = %v, want ErrNotFound", err)
	}
	st := TaskState{
		Goal:  "write a file",
		Facts: []Fact{{Summary: "read file X", Evidence: "contents", Tool: "read_file"}},
		Status: StatusRunning,
	}
	if err := j.SaveState("s", st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := j.LoadState("s")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Goal != "write a file" || got.Status != StatusRunning || len(got.Facts) != 1 {
		t.Fatalf("got = %+v", got)
	}
	if got.Facts[0].Summary != "read file X" {
		t.Fatalf("fact not preserved: %+v", got.Facts[0])
	}
}

func TestRestoreLoadsState(t *testing.T) {
	dir := t.TempDir()
	j, _ := NewJSONL(dir)
	defer j.Close()

	sess := NewSession("s-state")
	sess.Persistence = j
	sess.State = TaskState{Goal: "g", Facts: []Fact{{Summary: "f", Tool: "read_file"}}, Status: StatusRunning}
	if err := j.SaveState("s-state", sess.State); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	restored, err := Restore(j, "s-state")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.State.Goal != "g" || len(restored.State.Facts) != 1 {
		t.Fatalf("restored state = %+v, want Goal=facts preserved", restored.State)
	}
}
