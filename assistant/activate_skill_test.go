package assistant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hegq.net/mos/agent"
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