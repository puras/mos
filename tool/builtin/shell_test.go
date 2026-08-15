package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"hegq.net/mos/tool"
)

func TestShellExecute(t *testing.T) {
	res, err := Shell{}.Execute(context.Background(), json.RawMessage(`{"command":"echo hello-mos"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m := res.(map[string]any)
	if !strings.Contains(m["output"].(string), "hello-mos") {
		t.Fatalf("output = %v", m["output"])
	}
	if m["exit_code"].(int) != 0 {
		t.Fatalf("exit code = %v", m["exit_code"])
	}
}

// 沙箱路径（M1.6）：配置了执行沙箱时，Shell 在 WorkDir（jail）内执行。
func TestShellExecuteWithSandbox(t *testing.T) {
	dir := t.TempDir()
	ctx := tool.WithSandbox(context.Background(), &tool.Sandbox{WorkDir: dir})
	res, err := Shell{}.Execute(ctx, json.RawMessage(`{"command":"pwd"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m := res.(map[string]any)
	want, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(m["output"].(string), want) {
		t.Fatalf("output = %v, want contain %v", m["output"], want)
	}
}
