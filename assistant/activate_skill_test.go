package assistant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/session"
)

// writeFile 写测试 fixture（本地 helper；agent 包已有同名，跨包不便，直接复用）。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestActivateSkill_Success(t *testing.T) {
	skills := []agent.Skill{
		{
			Name:         "git",
			Description:  "git helper",
			Dir:          t.TempDir(),
			AllowedTools: []string{"git", "read_file"},
			References:   []string{"references/x.md"},
		},
	}
	writeFile(t, filepath.Join(skills[0].Dir, "SKILL.md"), "---\nname: git\ndescription: x\n---\n# Body\n")

	t1 := ActivateSkill{Skills: skills}
	out, err := t1.Execute(context.Background(), json.RawMessage(`{"name":"git"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out.(SkillActivationResult)
	if !ok {
		t.Fatalf("unexpected return type %T", out)
	}
	if got.Name != "git" {
		t.Errorf("Name = %q", got.Name)
	}
	if !strings.Contains(got.Body, "# Body") {
		t.Errorf("Body = %q", got.Body)
	}
}

func TestActivateSkill_NotFound(t *testing.T) {
	t1 := ActivateSkill{}
	if _, err := t1.Execute(context.Background(), json.RawMessage(`{"name":"missing"}`)); err == nil {
		t.Fatal("expected error")
	}
}

// TestActivateSkill_ToolSubset_Union 验证两次 Execute 复用同一 sess 时
// agent.AddTurnSkill 把两次 AllowedTools 取并集写入 sess.TurnToolSubset
//（spec §5.4）。两次 Execute 复用同一 ctx（含 WithSession 注入的 sess），
// 断言第二次后 sess.TurnToolSubset = [read_file, shell]，且 sess.TurnSkills
// 累积两条 ActivatedSkill（Name 字段保留注入顺序）。
func TestActivateSkill_ToolSubset_Union(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	writeFile(t, filepath.Join(dirA, "SKILL.md"), "---\nname: a\ndescription: x\n---\n# A\n")
	writeFile(t, filepath.Join(dirB, "SKILL.md"), "---\nname: b\ndescription: x\n---\n# B\n")

	skills := []agent.Skill{
		{Name: "a", Description: "x", Dir: dirA, AllowedTools: []string{"read_file"}},
		{Name: "b", Description: "x", Dir: dirB, AllowedTools: []string{"shell"}},
	}
	t1 := ActivateSkill{Skills: skills}

	sess := session.NewSession("test-union")
	ctx := WithSession(context.Background(), sess)

	if _, err := t1.Execute(ctx, json.RawMessage(`{"name":"a"}`)); err != nil {
		t.Fatalf("activate a: %v", err)
	}
	if _, err := t1.Execute(ctx, json.RawMessage(`{"name":"b"}`)); err != nil {
		t.Fatalf("activate b: %v", err)
	}

	subset := agent.ToolSubsetFromSession(sess)
	want := map[string]bool{"read_file": true, "shell": true}
	if len(subset) != len(want) {
		t.Fatalf("subset = %v, want %v", subset, want)
	}
	for _, n := range subset {
		if !want[n] {
			t.Errorf("unexpected %q in subset", n)
		}
	}

	activated := agent.SkillsFromSession(sess)
	if len(activated) != 2 || activated[0].Name != "a" || activated[1].Name != "b" {
		t.Errorf("activated = %+v, want [a b]", activated)
	}
}

// TestActivateSkill_NoSession_NoEvent 验证 ctx 中无 sess（未装配 / 纯单元测试）
// 时 activate_skill 仍返回 SkillActivationResult，但 sess 不被累积状态、事件
// 不写——优雅降级路径。
func TestActivateSkill_NoSession_NoEvent(t *testing.T) {
	dirA := t.TempDir()
	writeFile(t, filepath.Join(dirA, "SKILL.md"), "---\nname: a\ndescription: x\n---\n# A\n")
	skills := []agent.Skill{
		{Name: "a", Description: "x", Dir: dirA, AllowedTools: []string{"read_file"}},
	}
	t1 := ActivateSkill{Skills: skills}

	out, err := t1.Execute(context.Background(), json.RawMessage(`{"name":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out.(SkillActivationResult)
	if !ok {
		t.Fatalf("unexpected return type %T", out)
	}
	if got.Name != "a" {
		t.Errorf("Name = %q, want a", got.Name)
	}
	if got.AllowedTools[0] != "read_file" {
		t.Errorf("AllowedTools = %v", got.AllowedTools)
	}
}
