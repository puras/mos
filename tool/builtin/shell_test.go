package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos/sandbox"
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

// 沙箱路径（v2.24 capability seam）：注入 sandbox.Runner 后 Shell 在 WorkDir（jail）内执行。
func TestShellExecuteWithSandbox(t *testing.T) {
	dir := t.TempDir()
	ctx := sandbox.WithRunner(context.Background(), &sandbox.Local{WorkDir: dir})
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