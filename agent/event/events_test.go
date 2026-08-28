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

// TestFsResolveDataMarshal 钉住 fs/resolve 事件载荷契约（file-tool-jail Task 19）：
// 路径解析决策（allowed/denied）入会话日志，可回放审计。
func TestFsResolveDataMarshal(t *testing.T) {
	d := FsResolveData{
		Display:   "../../etc/passwd",
		Target:    "/etc/passwd",
		Op:        "read",
		Mode:      "saas-jailed",
		Decision:  "denied",
		Workspace: "/ws",
		Reason:    "路径越界",
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back FsResolveData
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Decision != "denied" || back.Op != "read" || back.Target != "/etc/passwd" {
		t.Fatalf("roundtrip mismatch: %+v", back)
	}
	if SessionEventFsResolve != "fs/resolve" {
		t.Fatalf("unexpected constant: %q", SessionEventFsResolve)
	}
}
