package event

import (
	"encoding/json"
	"testing"
)

func TestScriptEventDataMarshal(t *testing.T) {
	step := ScriptStepData{Turn: 3, StepID: "a", Tool: "find_files", Status: "done"}
	b, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ScriptStepData
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.StepID != "a" || back.Status != "done" {
		t.Fatalf("roundtrip mismatch: %+v", back)
	}
	if SessionEventScriptStart != "script/start" || SessionEventScriptEnd != "script/end" {
		t.Fatalf("unexpected constants: %q %q", SessionEventScriptStart, SessionEventScriptEnd)
	}
}