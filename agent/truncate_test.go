package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/puras/mos/agent/hook"
	"github.com/puras/mos/llm"
)

func lines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteByte('\n')
		}
		b.WriteString("line ")
		b.WriteString(strconv.Itoa(i)) // 行号递增，断言才分得清头尾
	}
	return b.String()
}

func TestTruncateNoOp(t *testing.T) {
	tr := NewDefaultTruncator(TruncatorConfig{})
	out, truncated := tr.Truncate("read_file", lines(10))
	if truncated || !strings.Contains(out, "line") {
		t.Fatalf("未超限不该截断: %v", truncated)
	}
}

func TestTruncateHeadForReadFile(t *testing.T) {
	tr := NewDefaultTruncator(TruncatorConfig{MaxLines: 10})
	out, truncated := tr.Truncate("read_file", lines(100))
	if !truncated {
		t.Fatal("应发生截断")
	}
	// 头部保留：开头在场、结尾不在
	if !strings.HasPrefix(out, "line 1\n") {
		t.Fatalf("read_file 应保留头部: %.40s", out)
	}
	if strings.Contains(out, "line 100") {
		t.Fatalf("尾部不该保留: %.80s", out)
	}
	if !strings.Contains(out, "[中段省略") {
		t.Fatalf("应有省略标记: %s", out)
	}
	if !strings.Contains(out, "已截断") || !strings.Contains(out, "100 行") {
		t.Fatalf("应说明截断了多少: %s", out)
	}
}

func TestTruncateTailForShell(t *testing.T) {
	tr := NewDefaultTruncator(TruncatorConfig{MaxLines: 10})
	out, truncated := tr.Truncate("shell", lines(100))
	if !truncated {
		t.Fatal("应发生截断")
	}
	if !strings.Contains(out, "line 100") {
		t.Fatalf("shell 应保留尾部（报错在最后）: %.80s", out)
	}
	if !strings.Contains(out, "[前面省略") {
		t.Fatalf("应有省略标记: %.80s", out)
	}
}

func TestTruncateBothDefault(t *testing.T) {
	tr := NewDefaultTruncator(TruncatorConfig{MaxLines: 10})
	out, truncated := tr.Truncate("custom_tool", lines(100))
	if !truncated {
		t.Fatal("应发生截断")
	}
	if !strings.HasPrefix(out, "line 1\n") || !strings.Contains(out, "line 100") {
		t.Fatalf("默认策略应两头各留: %.40s ... %.60s", out, out[len(out)-60:])
	}
}

func TestTruncateByteLimitUTF8Safe(t *testing.T) {
	tr := NewDefaultTruncator(TruncatorConfig{MaxBytes: 100})
	content := strings.Repeat("汉", 300) // 多字节：1800 字节
	out, truncated := tr.Truncate("read_file", content)
	if !truncated {
		t.Fatal("应发生截断")
	}
	// 去掉说明尾注后正文不出现半个字符（都能解码）
	if strings.Count(out, "\ufffd") != 0 {
		t.Fatalf("UTF-8 被切半: %q", out[:60])
	}
	if !strings.Contains(out, "已截断") {
		t.Fatalf("应有说明: %s", out)
	}
}

func TestTruncateSpillEscapeHatch(t *testing.T) {
	dir := t.TempDir()
	tr := NewDefaultTruncator(TruncatorConfig{MaxLines: 5, Dir: dir})
	out, truncated := tr.Truncate("shell", lines(50))
	if !truncated {
		t.Fatal("应发生截断")
	}
	if !strings.Contains(out, "全文已保存至 ") || !strings.Contains(out, "read_file(path=") {
		t.Fatalf("应给出落盘路径与回读方式: %s", out)
	}
	// 提取路径（到中文逗号为止）并确认文件真的写了全文
	i := strings.Index(out, "全文已保存至 ") + len("全文已保存至 ")
	j := strings.Index(out[i:], "，")
	if j < 0 {
		t.Fatalf("说明里应有路径: %s", out)
	}
	path := out[i : i+j]
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("落盘文件读不到: %v", err)
	}
	if len(full) != len(lines(50)) {
		t.Fatalf("落盘应为全文: %d != %d", len(full), len(lines(50)))
	}
	if !strings.HasPrefix(filepath.Base(path), "trunc-shell-") {
		t.Fatalf("文件名应带工具名: %s", path)
	}
}

func TestTruncateSpillFailureDegradesGracefully(t *testing.T) {
	// 目录是个文件：MkdirAll 必败 → 说明降级为"落盘失败"，截断本体不受影响
	file := filepath.Join(t.TempDir(), "blocker")
	os.WriteFile(file, []byte("x"), 0o644)
	tr := NewDefaultTruncator(TruncatorConfig{MaxLines: 5, Dir: filepath.Join(file, "sub")})
	out, truncated := tr.Truncate("shell", lines(50))
	if !truncated {
		t.Fatal("应发生截断")
	}
	if strings.Contains(out, "全文已保存至") {
		t.Fatalf("落盘失败不该给路径: %s", out)
	}
	if !strings.Contains(out, "全文落盘失败") {
		t.Fatalf("应诚实说明落盘失败: %s", out)
	}
}

func TestTruncateHookRewritesResultText(t *testing.T) {
	tr := NewDefaultTruncator(TruncatorConfig{MaxLines: 3})
	h := TruncateHook(tr)

	ev := &hook.HookEvent{
		Kind:       hook.HookToolPostExecute,
		ToolCall:   llm.ToolCall{ID: "c1", Name: "shell"},
		ResultText: lines(50),
	}
	out, err := h(t.Context(), ev, func(ctx context.Context, e *hook.HookEvent) (*hook.HookEvent, error) {
		return e, nil
	})
	if err != nil || out != ev {
		t.Fatalf("钩子应原路返回: %v", err)
	}
	if len(out.ResultText) >= len(lines(50)) {
		t.Fatalf("ResultText 应被截短")
	}

	// 未超限：原样透传
	ev2 := &hook.HookEvent{Kind: hook.HookToolPostExecute, ToolCall: llm.ToolCall{Name: "shell"}, ResultText: "ok"}
	out2, _ := h(t.Context(), ev2, func(ctx context.Context, e *hook.HookEvent) (*hook.HookEvent, error) { return e, nil })
	if out2.ResultText != "ok" {
		t.Fatalf("未超限不应改写: %q", out2.ResultText)
	}
}
