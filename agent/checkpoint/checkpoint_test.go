package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
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

// TestCheckpointScriptStateRoundtrip 验证 Scripted 驱动断点：ScriptState 经 FileStore（真实 json 落盘）
// 往返不丢值——若 ScriptState 的 tag 错（`-` / 拼错 / 缺 omitempty 行为），FileStore 会沉默丢失脚本态，
// resume 时无法续跑已完成的步骤。MemStore 仅做结构浅拷贝，不走 json 路径，故不作为本测试后端。
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
	st, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	ctx := context.Background()
	if err := st.Save(ctx, ckpt); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load(ctx, "scr1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ScriptState == nil {
		t.Fatal("ScriptState not persisted (json tag likely broken)")
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

// TestCheckpointBackwardCompatNoScriptState 验证老 checkpoint（react/planexecute 等）无 ScriptState 字段
// 时仍可正常落盘/读取。两条核心契约：
//  1. 反序列化：用一段字面量 legacy JSON（不包含 script_state 键）直接 Unmarshal，得到 ScriptState == nil
//     且其他字段完整还原——这是 on-disk 兼容性，对 MemStore（浅拷贝）无效。
//  2. 序列化：ScriptState 为 nil 的 Checkpoint 经 json.Marshal 后不出现 "script_state" 键——避免 Scripted
//     之外的驱动写出空壳字段污染老读取端。两边共同锁定 *T + omitempty 的正确性。
func TestCheckpointBackwardCompatNoScriptState(t *testing.T) {
	// 模拟"升级前已经写好的 on-disk 文件"：只含 Checkpoint 既有字段，无 script_state 键。
	legacyJSON := `{
		"session_id": "legacy1",
		"turn": 7,
		"step": 2,
		"hook_kind": "tool/pre-execute",
		"calls": [{"id":"c1","name":"write_file","args":"{}"}],
		"request": {"tool_name":"write_file","command":"a.txt","reason":"legacy"},
		"approval_id": "ap-legacy",
		"suspended_call_id": "c1",
		"decisions": {},
		"snapshot": {"goal":"legacy goal","status":"running"},
		"created_at": "2026-01-01T00:00:00Z"
	}`
	var got Checkpoint
	if err := json.Unmarshal([]byte(legacyJSON), &got); err != nil {
		t.Fatalf("legacy json unmarshal: %v", err)
	}
	if got.ScriptState != nil {
		t.Fatalf("ScriptState = %+v, want nil（向后兼容：老 json 无此键）", got.ScriptState)
	}
	if got.SessionID != "legacy1" || got.Turn != 7 || got.SuspendedCallID != "c1" || got.ApprovalID != "ap-legacy" {
		t.Fatalf("老字段还原错误: %+v", got)
	}

	// 反向：nil ScriptState 的 Checkpoint 不应在 json 中出现 script_state 键（omitempty 契约）。
	ckpt := sampleCheckpoint("marsh1")
	b, err := json.Marshal(ckpt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "script_state") {
		t.Fatalf("nil ScriptState 仍写入 json 字段，污染老读取端：%s", string(b))
	}
}
