package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puras/mos/sandbox"
	"github.com/puras/mos/tool"
)

// 未装配 sandbox → fail-closed：拒绝执行，不跑裸命令。
func TestShellWithoutRunnerFailsClosed(t *testing.T) {
	_, err := NewShell(nil).Execute(context.Background(), []byte(`{"command":"echo hi"}`))
	if err == nil {
		t.Fatal("未装配 sandbox 应拒绝执行")
	}
	var notCfg *sandbox.NotConfiguredError
	if !errors.As(err, &notCfg) {
		t.Fatalf("应返回 *sandbox.NotConfiguredError, got %T: %v", err, err)
	}
}

// 空命令 → 报错。
func TestShellEmptyCommand(t *testing.T) {
	s := NewShell(&sandbox.Local{})
	if _, err := s.Execute(context.Background(), []byte(`{"command":""}`)); err == nil {
		t.Fatal("空命令应报错")
	}
}

// 成功：回执带 output / exit_code / duration。
func TestShellSuccess(t *testing.T) {
	s := NewShell(&sandbox.Local{})
	out, err := s.Execute(context.Background(), []byte(`{"command":"echo hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if !strings.Contains(m["output"].(string), "hello") {
		t.Fatalf("output = %v", m["output"])
	}
	if m["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v", m["exit_code"])
	}
}

// 非零退出码 → **正常结果**（不是工具故障）：回执带 exit_code，输出末尾有
// [exit code: N] 标记。对齐 DSH：那里非零退出同样不是 isError。
// 理由：把非零退出当失败，模型就得自己写 hack 绕开（拼 `; echo "exit=$?"`
// 让整条命令以 0 退出）——实测它真会这么做。
func TestShellNonZeroExitIsNormalResult(t *testing.T) {
	s := NewShell(&sandbox.Local{})
	out, err := s.Execute(context.Background(), []byte(`{"command":"echo boom >&2; exit 2"}`))
	if err != nil {
		t.Fatalf("非零退出码不该是工具错误: %v", err)
	}
	m := out.(map[string]any)
	if m["exit_code"].(int) != 2 {
		t.Fatalf("exit_code = %v, want 2", m["exit_code"])
	}
	o := m["output"].(string)
	if !strings.Contains(o, "boom") {
		t.Fatalf("输出应照样给模型: %q", o)
	}
	if !strings.Contains(o, "[exit code: 2]") {
		t.Fatalf("输出末尾应带退出码标记: %q", o)
	}
}

// 输出超限 → 截断 + 落盘逃生（output_path 指向全文文件）。
func TestShellSpillsTruncatedOutput(t *testing.T) {
	dir := t.TempDir()
	s := NewShell(&sandbox.Local{MaxOutput: 512, SpillDir: dir})
	out, err := s.Execute(context.Background(),
		[]byte(`{"command":"head -c 5000 /dev/zero | tr '\\0' 'x'"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["truncated"] != true {
		t.Fatalf("应标记截断: %+v", m)
	}
	path, ok := m["output_path"].(string)
	if !ok || path == "" {
		t.Fatalf("截断应给出全文路径: %+v", m)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 5000 {
		t.Fatalf("落盘文件应含完整输出, got %d 字节", len(full))
	}
}

// 落盘目录由宿主在 Runner 上指定——默认的临时目录在文件 jail 的可写根之外。
func TestShellSpillDirIsHostChosen(t *testing.T) {
	dir := t.TempDir()
	s := NewShell(&sandbox.Local{MaxOutput: 512, SpillDir: dir})
	out, err := s.Execute(context.Background(),
		[]byte(`{"command":"head -c 5000 /dev/zero | tr '\\0' 'x'"}`))
	if err != nil {
		t.Fatal(err)
	}
	path := out.(map[string]any)["output_path"].(string)
	if !strings.HasPrefix(path, dir) {
		t.Fatalf("落盘应在宿主指定目录 %q 下，实际 %q", dir, path)
	}
}

// 超时：命令被终止，错误里带说明。
func TestShellTimeout(t *testing.T) {
	s := NewShell(&sandbox.Local{})
	_, err := s.Execute(context.Background(), []byte(`{"command":"sleep 30","timeout":1}`))
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("超时应报错: %v", err)
	}
}

// description 会被回填（审计友好）。
func TestShellEchoesDescription(t *testing.T) {
	s := NewShell(&sandbox.Local{})
	out, err := s.Execute(context.Background(),
		[]byte(`{"command":"echo x","description":"看一眼版本"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["description"] != "看一眼版本" {
		t.Fatalf("description 未回填: %+v", out)
	}
}

// shell 是写操作性质 → 独占（不声明可并发）。
func TestShellIsExclusive(t *testing.T) {
	s := NewShell(&sandbox.Local{})
	if _, ok := interface{}(s).(tool.ConcurrencySafe); ok {
		t.Fatal("shell 不应声明可并发")
	}
}

// 编译期断言与路径检查（落盘目录不存在时会自动创建）。
func TestShellSpillCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "spill")
	s := NewShell(&sandbox.Local{MaxOutput: 512, SpillDir: dir})
	out, err := s.Execute(context.Background(),
		[]byte(`{"command":"head -c 5000 /dev/zero | tr '\\0' 'x'"}`))
	if err != nil {
		t.Fatal(err)
	}
	path := out.(map[string]any)["output_path"].(string)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("落盘文件应存在: %v", err)
	}
}

// 模型没指定超时 → 用宿主的配置（工具不替宿主决定超时策略）。
func TestShellUsesHostTimeoutWhenModelOmitsIt(t *testing.T) {
	// 宿主的超时很短：若工具自己用 30s 缺省，这条测试会超时失败。
	sb := &sandbox.Local{Timeout: 300 * time.Millisecond}
	s := NewShell(sb)
	start := time.Now()
	_, err := s.Execute(context.Background(), []byte(`{"command":"sleep 30"}`))
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("应按宿主超时终止, 实际 %v", d)
	}
}
