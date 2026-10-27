package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/tool"
)

func jail(t *testing.T) (string, filesystem.FileSystem) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return root, filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
		ReadRoots:  []string{root},
		WriteRoots: []string{root},
	})
}

// 正常读：内容回填。
func TestReadInsideJail(t *testing.T) {
	root, fsys := jail(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewRead(fsys).Execute(context.Background(), []byte(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatalf("读取 jail 内文件: %v", err)
	}
	m := out.(map[string]any)
	if !strings.Contains(m["content"].(string), "hello") {
		t.Fatalf("content = %v", m["content"])
	}
	if m["total_lines"].(int) != 1 {
		t.Fatalf("total_lines = %v, want 1", m["total_lines"])
	}
}

// 越界读：返回 *PathDeniedError，且拒绝理由可回填给模型。
func TestReadOutsideJailDenied(t *testing.T) {
	_, fsys := jail(t)
	_, err := NewRead(fsys).Execute(context.Background(), []byte(`{"path":"../../etc/passwd"}`))
	if err == nil {
		t.Fatal("越界读应被拒绝")
	}
	var denied *filesystem.PathDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("应返回 *PathDeniedError，得到 %T: %v", err, err)
	}
}

// 未装配文件能力：fail-closed 拒绝，不放行。
func TestReadWithoutFilesystemFailsClosed(t *testing.T) {
	_, err := NewRead(nil).Execute(context.Background(), []byte(`{"path":"a.txt"}`))
	if err == nil {
		t.Fatal("未装配文件能力应拒绝")
	}
	var notCfg *filesystem.NotConfiguredError
	if !errors.As(err, &notCfg) {
		t.Fatalf("应返回 *NotConfiguredError，得到 %T: %v", err, err)
	}
}

// 正常写：落盘。
func TestWriteInsideJail(t *testing.T) {
	root, fsys := jail(t)
	out, err := NewWrite(fsys).Execute(context.Background(),
		[]byte(`{"path":"sub/b.txt","content":"data"}`))
	if err != nil {
		t.Fatalf("写入 jail 内文件: %v", err)
	}
	if out.(map[string]any)["bytes"].(int) != 4 {
		t.Fatalf("out = %v", out)
	}
	got, err := os.ReadFile(filepath.Join(root, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Fatalf("落盘内容 = %q", got)
	}
}

// 越界写：拒绝且**不落盘**（不能只看错误，还得看副作用没发生）。
func TestWriteOutsideJailDenied(t *testing.T) {
	root, fsys := jail(t)
	outside := filepath.Join(t.TempDir(), "evil.txt")
	_, err := NewWrite(fsys).Execute(context.Background(),
		[]byte(`{"path":"`+outside+`","content":"pwn"}`))
	if err == nil {
		t.Fatal("越界写应被拒绝")
	}
	var denied *filesystem.PathDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("应返回 *PathDeniedError，得到 %T: %v", err, err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("被拒的写入不应落盘")
	}
	_ = root
}

// 经越界 symlink 写：拒绝且不落盘。
func TestWriteThroughSymlinkDenied(t *testing.T) {
	root, fsys := jail(t)
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(root, "escape")); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}
	_, err := NewWrite(fsys).Execute(context.Background(),
		[]byte(`{"path":"escape/new.txt","content":"pwn"}`))
	if err == nil {
		t.Fatal("经越界 symlink 写应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("被拒的写入不应落到 jail 外")
	}
}

// FileTools 返回读 + 写 + 精确编辑三个工具。
func TestFileTools(t *testing.T) {
	_, fsys := jail(t)
	tools := FileTools(fsys, nil)
	if len(tools) != 3 {
		t.Fatalf("工具数 = %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name()] = true
	}
	if !names["read"] || !names["write"] || !names["edit"] {
		t.Fatalf("工具名 = %v", names)
	}
}

// 工具描述是模型唯一的使用指南（MoS 暂未提供“工具自带的系统提示贡献”通道），
// 所以分段读的正解必须留在描述里：调大 limit 一次读完，或一次发出多个 offset
// 的调用并行读完。描述被改写时这条会失败——防止指南被无意丢掉。
func TestReadDescriptionKeepsUsageGuide(t *testing.T) {
	desc := NewRead(nil).Description()
	for _, want := range []string{"limit", "offset", "并行", "行"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("read 描述应包含 %q（使用指南），实际：%s", want, desc)
		}
	}
}

// 默认上限 2000 行：中等文件一次读完（往返少），不再一小段一小段地磨。
func TestReadDefaultLimitReadsMediumFileWhole(t *testing.T) {
	root, fsys := jail(t)
	body := strings.Repeat("0123456789\n", 500) // 500 行 < 2000 行
	if err := os.WriteFile(filepath.Join(root, "mid.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewRead(fsys).Execute(context.Background(), []byte(`{"path":"mid.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if !strings.Contains(m["content"].(string), "0123456789") {
		t.Fatalf("内容缺失: %q", m["content"])
	}
	if m["total_lines"].(int) != 500 {
		t.Fatalf("total_lines = %v, want 500", m["total_lines"])
	}
	if m["next_offset"].(int) != 0 {
		t.Fatalf("读完时 next_offset 应为 0, got %v", m["next_offset"])
	}
	if !strings.Contains(m["content"].(string), "(End of file - total 500 lines)") {
		t.Fatalf("读完应有结束标记: %q", m["content"])
	}
}

// 行寻址：offset 是行号（1 起），limit 是行数。
func TestReadLineAddressing(t *testing.T) {
	root, fsys := jail(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"),
		[]byte("l1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewRead(fsys).Execute(context.Background(),
		[]byte(`{"path":"a.txt","offset":2,"limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	content := m["content"].(string)
	if !strings.Contains(content, "l2") || !strings.Contains(content, "l3") {
		t.Fatalf("应读到第 2-3 行: %q", content)
	}
	if strings.Contains(content, "l1") || strings.Contains(content, "l4") {
		t.Fatalf("不该读到窗口外的行: %q", content)
	}
	if m["offset"].(int) != 2 {
		t.Fatalf("offset = %v, want 2", m["offset"])
	}
	if m["total_lines"].(int) != 5 {
		t.Fatalf("total_lines = %v, want 5", m["total_lines"])
	}
	if !strings.Contains(content, "Use offset=4 to continue") {
		t.Fatalf("续读提示缺失: %q", content)
	}
	if m["next_offset"].(int) != 4 {
		t.Fatalf("next_offset = %v, want 4", m["next_offset"])
	}
}

// offset 越界 → 报错（PI 同）。
func TestReadOffsetBeyondEnd(t *testing.T) {
	root, fsys := jail(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("l1\nl2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRead(fsys).Execute(context.Background(),
		[]byte(`{"path":"a.txt","offset":99}`)); err == nil {
		t.Fatal("offset 越界应报错")
	}
}

// next_offset：下一段的起始**行号**；读完为 0（模型不用自己算）。
func TestReadNextOffset(t *testing.T) {
	root, fsys := jail(t)
	if err := os.WriteFile(filepath.Join(root, "big.txt"),
		[]byte(strings.Repeat("line\n", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewRead(fsys).Execute(context.Background(),
		[]byte(`{"path":"big.txt","limit":40}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["next_offset"].(int) != 41 {
		t.Fatalf("next_offset = %v, want 41", m["next_offset"])
	}
	if m["total_lines"].(int) != 100 {
		t.Fatalf("total_lines = %v, want 100", m["total_lines"])
	}
	out2, err := NewRead(fsys).Execute(context.Background(),
		[]byte(`{"path":"big.txt","offset":41,"limit":40}`))
	if err != nil {
		t.Fatal(err)
	}
	if out2.(map[string]any)["next_offset"].(int) != 81 {
		t.Fatalf("next_offset = %v, want 81", out2.(map[string]any)["next_offset"])
	}
	out3, err := NewRead(fsys).Execute(context.Background(),
		[]byte(`{"path":"big.txt","offset":81,"limit":40}`))
	if err != nil {
		t.Fatal(err)
	}
	if out3.(map[string]any)["next_offset"].(int) != 0 {
		t.Fatalf("读完时 next_offset 应为 0, got %v", out3.(map[string]any)["next_offset"])
	}
}

// write 的结果要告诉模型「新建还是覆盖、覆盖掉了多少」——没有这个信息，模型
// 只能读回来确认，反复重写就容易空转（DSH 区分 Created/Updated，PI 报告改了几处）。
func TestWriteReportsCreatedAndReplaced(t *testing.T) {
	root, fsys := jail(t)
	w := NewWrite(fsys)

	// 新建
	out, err := w.Execute(context.Background(), []byte(`{"path":"new.txt","content":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["created"] != true {
		t.Fatalf("新建时 created 应为 true: %+v", m)
	}
	if m["replaced_bytes"].(int64) != 0 {
		t.Fatalf("新建时 replaced_bytes 应为 0: %+v", m)
	}

	// 覆盖已有文件（原 5 字节）
	out, err = w.Execute(context.Background(), []byte(`{"path":"new.txt","content":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	m = out.(map[string]any)
	if m["created"] != false {
		t.Fatalf("覆盖时 created 应为 false: %+v", m)
	}
	if m["replaced_bytes"].(int64) != 5 {
		t.Fatalf("覆盖时 replaced_bytes 应为原长度 5: %+v", m)
	}

	// 覆盖：原有内容全部被丢弃，replaced_bytes = 原长度
	out, err = w.Execute(context.Background(), []byte(`{"path":"new.txt","content":"hey"}`))
	if err != nil {
		t.Fatal(err)
	}
	m = out.(map[string]any)
	if m["created"] != false || m["replaced_bytes"].(int64) != 2 {
		t.Fatalf("覆盖时 created=false 且 replaced_bytes=原长度 2: %+v", m)
	}
	got, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hey" {
		t.Fatalf("文件内容 = %q, want %q", got, "hey")
	}
	// 没有 append 参数了：传了也应被忽略而不是静默追加
	if _, ok := m["append"]; ok {
		t.Fatalf("回执不应再含 append 字段: %+v", m)
	}
}

// 单行超长保护：minified 文件的一行可能有几百 KB，只靠总量上限会把预算吃光。
func TestReadTruncatesLongLine(t *testing.T) {
	root, fsys := jail(t)
	if err := os.WriteFile(filepath.Join(root, "min.js"), []byte(strings.Repeat("y", 10000)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewRead(fsys).Execute(context.Background(), []byte(`{"path":"min.js"}`))
	if err != nil {
		t.Fatal(err)
	}
	content := out.(map[string]any)["content"].(string)
	if !strings.Contains(content, "line truncated to 2000 chars") {
		t.Fatalf("超长行应被截断并标记: %q", content[:80])
	}
}

// FileToolsIn：jail 根 = root，越界被拒；写前观察门生效（没读过不许覆盖）。
func TestFileToolsInJailsAndObserves(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tools, err := FileToolsIn(root)
	if err != nil {
		t.Fatalf("FileToolsIn: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("应有 3 个文件工具, got %d", len(tools))
	}
	var read, write tool.Tool
	for _, tl := range tools {
		switch tl.Name() {
		case "read":
			read = tl
		case "write":
			write = tl
		}
	}

	// 越界读：被 jail 拒绝。
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":`+strconv.Quote(outFile)+`}`)); err == nil {
		t.Fatal("越界路径应被拒绝")
	}

	// 没读过的文件：写前观察门拒绝覆盖。
	target := filepath.Join(root, "a.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Execute(context.Background(), json.RawMessage(`{"path":`+strconv.Quote(target)+`,"content":"new"}`)); err == nil {
		t.Fatal("没读过的文件应被写前观察门拒绝")
	}
}

// 运行期加根：同一套文件工具，边界跟着活根集合变（初始只有 A，加根后 B 也行）。
func TestFileToolsInRootsRuntimeAdd(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	outFile := filepath.Join(extra, "secret.txt")
	if err := os.WriteFile(outFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	roots := filesystem.NewRootSet([]string{root}, []string{root})
	tools, err := FileToolsInRoots(roots)
	if err != nil {
		t.Fatalf("FileToolsInRoots: %v", err)
	}
	var read tool.Tool
	for _, tl := range tools {
		if tl.Name() == "read" {
			read = tl
		}
	}

	// 加根前：越界读被拒。
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":`+strconv.Quote(outFile)+`}`)); err == nil {
		t.Fatal("加根前越界读应被拒绝")
	}

	// 运行期加根 → 立刻可读（边界每次调用现判）。
	roots.Add(extra)
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":`+strconv.Quote(outFile)+`}`)); err != nil {
		t.Fatalf("加根后应可读: %v", err)
	}

	// 根集合为 nil：报错（没有根就没有边界）。
	if _, err := FileToolsInRoots(nil); err == nil {
		t.Fatal("nil 根集合应报错")
	}
}
