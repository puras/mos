package checkpoint

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
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
