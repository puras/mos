package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/tool"
)

func editTool(t *testing.T) (string, *Edit) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return root, NewEdit(filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
		ReadRoots:  []string{root},
		WriteRoots: []string{root},
	}))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 唯一匹配 → 替换成功，回执带替换处数与起始行。
func TestEditReplacesUniqueMatch(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.go")
	writeFile(t, target, "package main\n\nfunc main() {\n\tprintln(1)\n}\n")

	out, err := e.Execute(context.Background(),
		[]byte(`{"path":"a.go","edits":[{"old_string":"println(1)","new_string":"println(2)"}]}`))
	if err != nil {
		t.Fatalf("唯一匹配应替换成功: %v", err)
	}
	m := out.(map[string]any)
	if m["replacements"].(int) != 1 {
		t.Fatalf("replacements = %v, want 1", m["replacements"])
	}
	if m["first_changed_line"].(int) != 4 {
		t.Fatalf("first_changed_line = %v, want 4", m["first_changed_line"])
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "println(2)") || strings.Contains(string(got), "println(1)") {
		t.Fatalf("内容未替换: %q", got)
	}
}

// old_string 出现多次 → 报错并报出次数，且**不落盘**。
func TestEditAmbiguousMatchRejected(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.go")
	writeFile(t, target, "x := 1\nx := 1\n")
	_, err := e.Execute(context.Background(),
		[]byte(`{"path":"a.go","edits":[{"old_string":"x := 1","new_string":"x := 2"}]}`))
	if err == nil {
		t.Fatal("多处匹配应报错")
	}
	if !strings.Contains(err.Error(), "2 次") {
		t.Fatalf("错误应报出出现次数: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "x := 1\nx := 1\n" {
		t.Fatalf("被拒的编辑不应落盘: %q", got)
	}
}

// 多处修改：一次调用改多个地方，全部按**原文**匹配。
func TestEditAppliesMultipleEdits(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.go")
	writeFile(t, target, "alpha\nbeta\ngamma\n")

	out, err := e.Execute(context.Background(), []byte(`{"path":"a.go","edits":[`+
		`{"old_string":"alpha","new_string":"ALPHA"},`+
		`{"old_string":"gamma","new_string":"GAMMA"}]}`))
	if err != nil {
		t.Fatalf("多处编辑应成功: %v", err)
	}
	m := out.(map[string]any)
	if m["replacements"].(int) != 2 {
		t.Fatalf("replacements = %v, want 2", m["replacements"])
	}
	if m["first_changed_line"].(int) != 1 {
		t.Fatalf("first_changed_line = %v, want 1", m["first_changed_line"])
	}
	got, _ := os.ReadFile(target)
	if string(got) != "ALPHA\nbeta\nGAMMA\n" {
		t.Fatalf("内容 = %q", got)
	}
}

// 两条编辑重叠 → 报错（PI：改同一片区域就合并成一条），且不落盘。
func TestEditRejectsOverlappingEdits(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.go")
	writeFile(t, target, "one two three\n")
	_, err := e.Execute(context.Background(), []byte(`{"path":"a.go","edits":[`+
		`{"old_string":"one two","new_string":"1"},`+
		`{"old_string":"two three","new_string":"3"}]}`))
	if err == nil || !strings.Contains(err.Error(), "重叠") {
		t.Fatalf("重叠编辑应报错: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "one two three\n" {
		t.Fatalf("被拒的编辑不应落盘: %q", got)
	}
}

// 容错：模型把 edits 发成单个对象或 JSON 字符串时也收下（PI 的实战经验）。
func TestEditToleratesSingleObjectOrJSONString(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.go")
	writeFile(t, target, "alpha\n")

	// 单个对象
	if _, err := e.Execute(context.Background(),
		[]byte(`{"path":"a.go","edits":{"old_string":"alpha","new_string":"ALPHA"}}`)); err != nil {
		t.Fatalf("单对象形状应被接受: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "ALPHA\n" {
		t.Fatalf("内容 = %q", got)
	}
	// JSON 字符串
	writeFile(t, target, "alpha\n")
	if _, err := e.Execute(context.Background(),
		[]byte(`{"path":"a.go","edits":"[{\"old_string\":\"alpha\",\"new_string\":\"BETA\"}]"}`)); err != nil {
		t.Fatalf("JSON 字符串形状应被接受: %v", err)
	}
	got, _ = os.ReadFile(target)
	if string(got) != "BETA\n" {
		t.Fatalf("内容 = %q", got)
	}
}

// old_string 找不到 → 报错，不落盘。
func TestEditNotFound(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.go")
	writeFile(t, target, "package main\n")
	_, err := e.Execute(context.Background(),
		[]byte(`{"path":"a.go","edits":[{"old_string":"不存在的片段","new_string":"x"}]}`))
	if err == nil || !strings.Contains(err.Error(), "没有找到") {
		t.Fatalf("找不到应报错: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "package main\n" {
		t.Fatalf("被拒的编辑不应落盘: %q", got)
	}
}

// 文件不存在 → 报错（edit 不创建文件，创建是 write 的活）。
func TestEditMissingFile(t *testing.T) {
	_, e := editTool(t)
	_, err := e.Execute(context.Background(),
		[]byte(`{"path":"nope.txt","edits":[{"old_string":"a","new_string":"b"}]}`))
	if err == nil {
		t.Fatal("文件不存在应报错")
	}
}

// 参数校验：old_string 非空、且与 new_string 不同。
func TestEditValidatesParams(t *testing.T) {
	_, e := editTool(t)
	cases := []string{
		`{"path":"a.txt","edits":[{"old_string":"","new_string":"b"}]}`,
		`{"path":"a.txt","edits":[{"old_string":"a","new_string":"a"}]}`,
		`{"path":"","edits":[{"old_string":"a","new_string":"b"}]}`,
		`{"path":"a.txt","edits":[]}`,
	}
	for _, args := range cases {
		if _, err := e.Execute(context.Background(), []byte(args)); err == nil {
			t.Fatalf("参数非法应报错: %s", args)
		}
	}
}

// 未装配文件能力 → fail-closed。
func TestEditWithoutFilesystemFailsClosed(t *testing.T) {
	_, err := NewEdit(nil).Execute(context.Background(),
		[]byte(`{"path":"a.txt","edits":[{"old_string":"a","new_string":"b"}]}`))
	if err == nil {
		t.Fatal("未装配文件能力应拒绝")
	}
	var notCfg *filesystem.NotConfiguredError
	if !errors.As(err, &notCfg) {
		t.Fatalf("应返回 *NotConfiguredError, got %T", err)
	}
}

// 越界路径 → 走写 jail，被拒。
func TestEditOutsideJailDenied(t *testing.T) {
	_, e := editTool(t)
	outside := filepath.Join(t.TempDir(), "evil.txt")
	writeFile(t, outside, "a")
	_, err := e.Execute(context.Background(),
		[]byte(`{"path":"`+outside+`","edits":[{"old_string":"a","new_string":"b"}]}`))
	if err == nil {
		t.Fatal("越界编辑应被拒绝")
	}
	var denied *filesystem.PathDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("应返回 *PathDeniedError, got %T: %v", err, err)
	}
}

// CRLF 文件：old_string 用 LF 也能匹配，写回后行尾仍是 CRLF。
func TestEditPreservesCRLF(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.txt")
	writeFile(t, target, "line1\r\nline2\r\n")
	if _, err := e.Execute(context.Background(),
		[]byte(`{"path":"a.txt","edits":[{"old_string":"line2","new_string":"line2 edited"}]}`)); err != nil {
		t.Fatalf("CRLF 文件应能匹配: %v", err)
	}
	got, _ := os.ReadFile(target)
	if !strings.Contains(string(got), "line2 edited") {
		t.Fatalf("未替换: %q", got)
	}
	if !strings.Contains(string(got), "\r\n") {
		t.Fatalf("行尾应保持 CRLF: %q", got)
	}
	if strings.Contains(string(got), "edited\r\n\r\n") {
		t.Fatalf("不应多出行尾: %q", got)
	}
}

// BOM 文件：匹配不受 BOM 影响，写回后 BOM 保留。
func TestEditPreservesBOM(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.txt")
	writeFile(t, target, "\ufeffpackage main\n")
	if _, err := e.Execute(context.Background(),
		[]byte(`{"path":"a.txt","edits":[{"old_string":"package main","new_string":"package other"}]}`)); err != nil {
		t.Fatalf("BOM 文件应能匹配: %v", err)
	}
	got, _ := os.ReadFile(target)
	if !strings.HasPrefix(string(got), "\ufeff") {
		t.Fatalf("BOM 应保留: %q", got)
	}
	if !strings.Contains(string(got), "package other") {
		t.Fatalf("未替换: %q", got)
	}
}

// 二进制文件 → 拒绝。
func TestEditRejectsBinary(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.bin")
	if err := os.WriteFile(target, []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(context.Background(),
		[]byte(`{"path":"a.bin","edits":[{"old_string":"x","new_string":"y"}]}`)); err == nil {
		t.Fatal("二进制文件应拒绝编辑")
	}
}

// edit 是写操作 → 独占（不声明可并发），与 write 同一规矩。
func TestEditIsExclusive(t *testing.T) {
	_, e := editTool(t)
	if _, ok := interface{}(e).(tool.ConcurrencySafe); ok {
		t.Fatal("edit 是写操作，不应声明可并发")
	}
}

// 原子写：写完的文件不留临时文件。
func TestAtomicWriteLeavesNoTempFile(t *testing.T) {
	root, e := editTool(t)
	target := filepath.Join(root, "a.txt")
	writeFile(t, target, "old\n")
	if _, err := e.Execute(context.Background(),
		[]byte(`{"path":"a.txt","edits":[{"old_string":"old","new_string":"new"}]}`)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".mos-write-") {
			t.Fatalf("临时文件残留: %s", en.Name())
		}
	}
}
