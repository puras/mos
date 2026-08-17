package checkpoint

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
	"hegq.net/mos/planner"
)

func sampleCheckpoint(sid string) *Checkpoint {
	return &Checkpoint{
		SessionID: sid,
		Turn:      3,
		Step:      1,
		HookKind:  "tool/pre-execute",
		Calls:     []llm.ToolCall{{ID: "c1", Name: "write_file", Args: `{"path":"a.txt"}`}},
		Request: approval.ApprovalRequest{
			ToolName: "write_file",
			Command:  "a.txt",
			Reason:   "写入文件（工作目录内，执行前确认）",
		},
		ApprovalID:      "ap-1",
		SuspendedCallID: "c1",
		Decisions:       map[string]approval.ApprovalDecision{},
		Snapshot:        session.TaskState{Goal: "write file", Status: session.StatusRunning},
	}
}

func TestMemStoreRoundTrip(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	ckpt := sampleCheckpoint("s1")
	if err := s.Save(ctx, ckpt); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Turn != 3 || got.Snapshot.Goal != "write file" || got.SuspendedCallID != "c1" {
		t.Fatalf("got = %+v", got)
	}
	if err := s.Delete(ctx, "s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load after delete err = %v, want ErrNotFound", err)
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()
	ckpt := sampleCheckpoint("s2")
	if err := s.Save(ctx, ckpt); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, "s2")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ApprovalID != "ap-1" || got.Turn != 3 {
		t.Fatalf("got = %+v", got)
	}
	if _, err := s.Load(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load missing err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "s2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 文件确实删除
	if _, err := s.Load(ctx, "s2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load after delete err = %v", err)
	}
}

func TestFileStoreAtomicNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	ctx := context.Background()
	_ = s.Save(ctx, sampleCheckpoint("s3"))
	matches, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("文件数 = %d, want 1（无 .tmp 残留）", len(matches))
	}
}

func TestFileStoreOverwrite(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	ctx := context.Background()
	c1 := sampleCheckpoint("s4")
	c1.Turn = 1
	if err := s.Save(ctx, c1); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	c2 := sampleCheckpoint("s4")
	c2.Turn = 2
	if err := s.Save(ctx, c2); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	got, err := s.Load(ctx, "s4")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Turn != 2 {
		t.Fatalf("overwrite 后 Turn = %d, want 2", got.Turn)
	}
}

// TestCheckpointScriptStateRoundtrip 验证 Scripted 驱动断点：ScriptState 经 MemStore 往返不丢字段。
// 字段说明：Plan=脚本定义、Outputs=已完成步骤的命名输出、Status=每步运行状态（resume 时按此续跑）。
func TestCheckpointScriptStateRoundtrip(t *testing.T) {
	plan := planner.ScriptedPlan{
		Goal: "g",
		Steps: []planner.ScriptStep{
			{ID: "a", Tool: "find_files", Args: map[string]any{"dir": "."}, Output: "files"},
		},
	}
	ckpt := &Checkpoint{
		SessionID: "scr1",
		Turn:      2,
		Step:      1,
		HookKind:  hook.HookToolPreExecute,
		Calls:     []llm.ToolCall{{ID: "step:a", Name: "find_files", Args: `{"dir":"."}`}},
		Request: approval.ApprovalRequest{
			ToolName: "find_files",
			Command:  ".",
			Reason:   "扫描工作目录（执行前确认）",
		},
		SuspendedCallID: "step:a",
		Decisions:       map[string]approval.ApprovalDecision{},
		Snapshot:        session.TaskState{Goal: "g"},
		ScriptState: &planner.ScriptState{
			Plan:    plan,
			Outputs: map[string]any{"a": "file1.txt"},
			Status:  map[string]string{"a": "running"},
		},
	}
	st := NewMemStore()
	ctx := context.Background()
	if err := st.Save(ctx, ckpt); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load(ctx, "scr1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ScriptState == nil {
		t.Fatal("ScriptState not persisted")
	}
	if got.ScriptState.Status["a"] != "running" {
		t.Fatalf("Status mismatch: %+v", got.ScriptState.Status)
	}
	if got.ScriptState.Outputs["a"] != "file1.txt" {
		t.Fatalf("Outputs mismatch: %+v", got.ScriptState.Outputs)
	}
	if len(got.ScriptState.Plan.Steps) != 1 {
		t.Fatalf("plan steps len = %d, want 1: %+v", len(got.ScriptState.Plan.Steps), got.ScriptState.Plan)
	}
	if got.ScriptState.Plan.Steps[0].ID != "a" || got.ScriptState.Plan.Steps[0].Tool != "find_files" {
		t.Fatalf("plan step[0] mismatch: %+v", got.ScriptState.Plan.Steps[0])
	}
}

// TestCheckpointBackwardCompat_NoScriptState 验证老 checkpoint（react/planexecute 等）无 ScriptState 字段
// 时仍可正常落盘/读取，且加载后 ScriptState 为 nil——这是变更的核心契约：向后兼容。
func TestCheckpointBackwardCompat_NoScriptState(t *testing.T) {
	ckpt := sampleCheckpoint("old1") // 不含 ScriptState
	st := NewMemStore()
	ctx := context.Background()
	if err := st.Save(ctx, ckpt); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load(ctx, "old1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ScriptState != nil {
		t.Fatalf("ScriptState = %+v, want nil（向后兼容）", got.ScriptState)
	}
	// 现有字段仍可读
	if got.SuspendedCallID != "c1" || got.Turn != 3 {
		t.Fatalf("老字段被破坏: %+v", got)
	}
}
