package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
)

func TestSystemAssembler_EmitsSkillLoad(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "SKILL.md"), skillDocFull)

	sess := session.NewSession("test")
	sess.State.Goal = "test"

	sa := NewDefaultSystemAssembler(SystemAssemblerConfig{
		Constraints: "x",
		SkillsDir:   dir,
		EnableSlots: []SystemSlot{SlotSkills},
	})

	if _, err := sa.Assemble(context.Background(), sess, nil); err != nil {
		t.Fatal(err)
	}

	hasLoad := false
	for _, e := range sess.Events {
		if e.Type == event.SessionEventSkillLoad {
			hasLoad = true
			var d event.SkillLoadData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(d.Skills) != 1 {
				t.Fatalf("expected 1 skill, got %d", len(d.Skills))
			}
			if d.Skills[0].Name != "git-commit-helper" {
				t.Errorf("Name = %q", d.Skills[0].Name)
			}
			if d.Skills[0].SchemaVersion != "1" {
				t.Errorf("SchemaVersion = %q", d.Skills[0].SchemaVersion)
			}
		}
	}
	if !hasLoad {
		t.Errorf("missing skill/load event in session.Events")
	}
}
