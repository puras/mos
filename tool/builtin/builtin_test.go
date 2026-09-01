package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCurrentTimeExecute(t *testing.T) {
	res, err := CurrentTime{}.Execute(context.Background(), json.RawMessage(`{"timezone":"UTC"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m := res.(map[string]any)
	if m["timezone"] != "UTC" {
		t.Fatalf("timezone = %v", m["timezone"])
	}
	if _, ok := m["time"].(string); !ok {
		t.Fatalf("time missing: %+v", m)
	}
	// 无效时区应报错
	if _, err := (CurrentTime{}).Execute(context.Background(), json.RawMessage(`{"timezone":"Mars/Olympus"}`)); err == nil {
		t.Fatal("invalid timezone should fail")
	}
}

// Default 应包含且仅包含 6 个内置工具（v2.24：web_fetch 已迁至 tool/extension）。
// 文件工具的 jail 行为见 file_jail_test.go（详尽边界测试）。
func TestDefault(t *testing.T) {
	names := map[string]bool{}
	for _, tl := range Default() {
		if names[tl.Name()] {
			t.Fatalf("duplicate tool %q in Default", tl.Name())
		}
		names[tl.Name()] = true
	}
	want := []string{"shell", "current_time", "read_file", "write_file", "glob", "grep"}
	if len(names) != len(want) {
		t.Fatalf("Default() has %d tools, want %d (%v)", len(names), len(want), names)
	}
	for _, n := range want {
		if !names[n] {
			t.Fatalf("Default() missing %q", n)
		}
	}
}