package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos/filesystem"
)

// 搜索三件套的测试：grep（内容）/ glob（文件名）/ ls（目录）。
//
// 关注三件事：结果对不对、边界守不守（jail）、上限收不收（limit）。

// grep：命中行带行号，格式 path:行号:内容。
func TestGrepMatches(t *testing.T) {
	root, fsys := jail(t)
	mustWrite(t, filepath.Join(root, "a.go"), "package main\n\nfunc main() {}\n")
	mustWrite(t, filepath.Join(root, "b.txt"), "nothing here\n")

	out, err := NewGrep(fsys).Execute(context.Background(), []byte(`{"pattern":"func main"}`))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	m := out.(map[string]any)
	matches := m["matches"].([]string)
	if len(matches) != 1 {
		t.Fatalf("命中 %d 条, want 1: %v", len(matches), matches)
	}
	if !strings.Contains(matches[0], "a.go:3:") {
		t.Fatalf("匹配行格式应为 path:行号:内容, got %q", matches[0])
	}
	if m["files_scanned"].(int) != 2 {
		t.Fatalf("files_scanned = %v, want 2", m["files_scanned"])
	}
}

// grep：glob 过滤只搜匹配的文件名。
func TestGrepGlobFilter(t *testing.T) {
	root, fsys := jail(t)
	mustWrite(t, filepath.Join(root, "a.go"), "target\n")
	mustWrite(t, filepath.Join(root, "b.txt"), "target\n")

	out, err := NewGrep(fsys).Execute(context.Background(), []byte(`{"pattern":"target","glob":"*.go"}`))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	matches := out.(map[string]any)["matches"].([]string)
	if len(matches) != 1 || !strings.Contains(matches[0], "a.go") {
		t.Fatalf("glob 过滤后应只剩 a.go, got %v", matches)
	}
}

// grep：非法正则 → 报错回填（fail-closed，不静默当字面量）。
func TestGrepInvalidRegex(t *testing.T) {
	_, fsys := jail(t)
	_, err := NewGrep(fsys).Execute(context.Background(), []byte(`{"pattern":"a("}`))
	if err == nil {
		t.Fatal("非法正则应报错")
	}
	if !strings.Contains(err.Error(), "正则") {
		t.Fatalf("错误理由应说明是正则问题: %v", err)
	}
}

// grep：越界路径 → *PathDeniedError（jail 不因为"搜索"而放松）。
func TestGrepOutsideJailDenied(t *testing.T) {
	_, fsys := jail(t)
	_, err := NewGrep(fsys).Execute(context.Background(), []byte(`{"pattern":"x","path":"../../etc"}`))
	var denied *filesystem.PathDeniedError
	if err == nil {
		t.Fatal("越界搜索应被拒绝")
	}
	if !asPathDenied(err, &denied) {
		t.Fatalf("应返回 *PathDeniedError, got %T: %v", err, err)
	}
}

// grep：符号链接逃逸——根内指向外部的链接既不被跟随，也不被读到内容。
func TestGrepSymlinkEscapeSkipped(t *testing.T) {
	root, fsys := jail(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, outside, "TOP-SECRET\n")
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("本机不支持符号链接: %v", err)
	}
	out, err := NewGrep(fsys).Execute(context.Background(), []byte(`{"pattern":"TOP-SECRET"}`))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	matches := out.(map[string]any)["matches"].([]string)
	if len(matches) != 0 {
		t.Fatalf("符号链接内容不该被搜到: %v", matches)
	}
}

// grep：limit 收口 + truncated 标注（超量时如实说"被截了"）。
func TestGrepLimitTruncates(t *testing.T) {
	root, fsys := jail(t)
	mustWrite(t, filepath.Join(root, "a.txt"), "hit\nhit\nhit\nhit\n")
	out, err := NewGrep(fsys).Execute(context.Background(), []byte(`{"pattern":"hit","limit":2}`))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	m := out.(map[string]any)
	if len(m["matches"].([]string)) != 2 {
		t.Fatalf("limit 未生效: %v", m["matches"])
	}
	if !m["truncated"].(bool) {
		t.Fatal("超量时 truncated 应为 true")
	}
}

// glob：** 跨层、无 "/" 的模式按文件名匹配任意层级。
func TestGlobPatterns(t *testing.T) {
	root, fsys := jail(t)
	mustWrite(t, filepath.Join(root, "a.go"), "x")
	mustWrite(t, filepath.Join(root, "sub", "b.go"), "x")
	mustWrite(t, filepath.Join(root, "sub", "deep", "c.ts"), "x")

	cases := []struct {
		pattern string
		want    []string
	}{
		{"*.go", []string{"a.go", "sub/b.go"}},
		{"**/*.go", []string{"a.go", "sub/b.go"}},
		{"sub/**/*.ts", []string{"sub/deep/c.ts"}},
		{"*.ts", []string{"sub/deep/c.ts"}},
	}
	for _, c := range cases {
		out, err := NewGlob(fsys).Execute(context.Background(),
			[]byte(`{"pattern":"`+c.pattern+`"}`))
		if err != nil {
			t.Fatalf("glob %q: %v", c.pattern, err)
		}
		files := out.(map[string]any)["files"].([]string)
		if len(files) != len(c.want) {
			t.Fatalf("glob %q 命中 %d 个 (%v), want %d", c.pattern, len(files), files, len(c.want))
		}
		for i, f := range files {
			if !strings.HasSuffix(f, c.want[i]) {
				t.Fatalf("glob %q 命中 %q, want 后缀 %q", c.pattern, f, c.want[i])
			}
		}
	}
}

// glob：glob 到文件（不是目录）→ 明确报错。
func TestGlobOnFileFails(t *testing.T) {
	root, fsys := jail(t)
	mustWrite(t, filepath.Join(root, "a.txt"), "x")
	_, err := NewGlob(fsys).Execute(context.Background(), []byte(`{"pattern":"*","path":"a.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("应报“不是目录”, got %v", err)
	}
}

// ls：列目录，含隐藏项，按名称排序，带类型。
func TestLsListsEntries(t *testing.T) {
	root, fsys := jail(t)
	mustWrite(t, filepath.Join(root, "b.txt"), "12345")
	mustWrite(t, filepath.Join(root, ".hidden"), "x")
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := NewLs(fsys).Execute(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	m := out.(map[string]any)
	entries := m["entries"].([]map[string]any)
	if len(entries) != 3 {
		t.Fatalf("条目数 = %d, want 3: %v", len(entries), entries)
	}
	if entries[0]["name"] != ".hidden" {
		t.Fatalf("应按名称排序，首项 = %v", entries[0]["name"])
	}
	if entries[1]["name"] != "b.txt" || entries[1]["type"] != "file" {
		t.Fatalf("b.txt 应为 file: %v", entries[1])
	}
	if entries[2]["name"] != "sub" || entries[2]["type"] != "dir" {
		t.Fatalf("sub 应为 dir: %v", entries[2])
	}
	if entries[1]["size"].(int64) != 5 {
		t.Fatalf("size = %v, want 5", entries[1]["size"])
	}
}

// ls：ls 到文件 → 明确报错。
func TestLsOnFileFails(t *testing.T) {
	root, fsys := jail(t)
	mustWrite(t, filepath.Join(root, "a.txt"), "x")
	_, err := NewLs(fsys).Execute(context.Background(), []byte(`{"path":"a.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("应报“不是目录”, got %v", err)
	}
}

// 三个搜索工具都是只读：可并发（tool.ConcurrencySafe）。
func TestSearchToolsConcurrencySafe(t *testing.T) {
	_, fsys := jail(t)
	for _, tl := range SearchTools(fsys) {
		cs, ok := tl.(interface {
			ConcurrencySafe(json.RawMessage) bool
		})
		if !ok {
			t.Fatalf("%s 未声明 ConcurrencySafe", tl.Name())
		}
		if !cs.ConcurrencySafe(nil) {
			t.Fatalf("%s 应声明可并发（只读）", tl.Name())
		}
	}
}

// 未装配文件能力：不给搜索能力（返回空集），而不是给必然失败的工具。
func TestSearchToolsWithoutFilesystem(t *testing.T) {
	if got := SearchTools(nil); got != nil {
		t.Fatalf("无文件能力时应返回空集, got %d 个", len(got))
	}
}

// 宿主排除策略：被排除的文件既不参与 grep，也不进 glob 结果。
func TestSearchExcludePredicate(t *testing.T) {
	root, fsys := jail(t)
	mustWrite(t, filepath.Join(root, ".env"), "SECRET=1\n")
	mustWrite(t, filepath.Join(root, "ok.txt"), "SECRET=1\n")
	exclude := func(path string) bool { return strings.HasSuffix(path, ".env") }

	tools := SearchTools(fsys, WithExclude(exclude))
	var grepTool *Grep
	var globTool *Glob
	for _, tl := range tools {
		switch tt := tl.(type) {
		case *Grep:
			grepTool = tt
		case *Glob:
			globTool = tt
		}
	}
	if grepTool == nil || globTool == nil {
		t.Fatal("SearchTools 应返回 grep 与 glob")
	}
	out, err := grepTool.Execute(context.Background(), []byte(`{"pattern":"SECRET"}`))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	matches := out.(map[string]any)["matches"].([]string)
	if len(matches) != 1 || !strings.Contains(matches[0], "ok.txt") {
		t.Fatalf("被排除的文件不该被搜到: %v", matches)
	}
	out, err = globTool.Execute(context.Background(), []byte(`{"pattern":"*"}`))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	files := out.(map[string]any)["files"].([]string)
	for _, f := range files {
		if strings.HasSuffix(f, ".env") {
			t.Fatalf("被排除的文件不该出现在 glob 结果里: %v", files)
		}
	}
}

// glob 模式编译：字符组与花括号按 shell 习惯展开（`a[1].txt`、`*.{go,md}`）。
func TestGlobPatternEscapes(t *testing.T) {
	root, fsys := jail(t)
	mustWrite(t, filepath.Join(root, "a[1].txt"), "x")
	mustWrite(t, filepath.Join(root, "a1.txt"), "x")
	mustWrite(t, filepath.Join(root, "b.go"), "x")
	mustWrite(t, filepath.Join(root, "c.md"), "x")
	cases := []struct {
		pattern string
		want    []string
	}{
		{"a[[]1].txt", []string{"a[1].txt"}},    // 字符组里的字面量方括号
		{"a[1].txt", []string{"a1.txt"}},        // 字符组本身
		{"*.{go,md}", []string{"b.go", "c.md"}}, // 花括号择一
	}
	for _, c := range cases {
		out, err := NewGlob(fsys).Execute(context.Background(), []byte(`{"pattern":"`+c.pattern+`"}`))
		if err != nil {
			t.Fatalf("glob %q: %v", c.pattern, err)
		}
		files := out.(map[string]any)["files"].([]string)
		if len(files) != len(c.want) {
			t.Fatalf("glob %q 命中 %v, want %v", c.pattern, files, c.want)
		}
		for i, f := range files {
			if !strings.HasSuffix(f, c.want[i]) {
				t.Fatalf("glob %q 命中 %q, want %q", c.pattern, f, c.want[i])
			}
		}
	}
}

// mustWrite 写文件（测试辅助）。
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// asPathDenied 判断 err 是否是 *filesystem.PathDeniedError。
func asPathDenied(err error, out **filesystem.PathDeniedError) bool {
	d, ok := err.(*filesystem.PathDeniedError)
	if ok {
		*out = d
	}
	return ok
}
