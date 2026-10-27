package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/tool"
)

// 授权后边界真的变了：同一套文件工具，加根前拒绝、加根后放行。
func TestGrantRootOpensDirectory(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	if err := os.WriteFile(filepath.Join(b, "note.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	roots := filesystem.NewRootSet([]string{a}, []string{a})
	tools, err := FileToolsInRoots(roots)
	if err != nil {
		t.Fatalf("FileToolsInRoots: %v", err)
	}
	var read tool.Tool
	for _, tl := range tools {
		if tl.Name() == "read_file" {
			read = tl
		}
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":`+quote(filepath.Join(b, "note.txt"))+`}`)); err == nil {
		t.Fatal("授权前应被拒绝")
	}

	grant := NewGrantRoot(roots)
	out, err := grant.Execute(context.Background(), json.RawMessage(`{"path":`+quote(b)+`}`))
	if err != nil {
		t.Fatalf("授权失败: %v", err)
	}
	m := out.(map[string]any)
	if m["note"] != "已授权，现在可以访问该目录" {
		t.Fatalf("回执 = %v", m)
	}

	for _, tl := range tools {
		if tl.Name() == "read_file" {
			if _, err := tl.Execute(context.Background(), json.RawMessage(`{"path":`+quote(filepath.Join(b, "note.txt"))+`}`)); err != nil {
				t.Fatalf("授权后应可读: %v", err)
			}
		}
	}
}

// mode 决定授权范围：read 不给写权限。
func TestGrantRootModeSeparatesReadWrite(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	roots := filesystem.NewRootSet([]string{a}, []string{a})
	fsys := filesystem.NewSandboxedWithRoots(roots)
	ctx := context.Background()

	if _, err := NewGrantRoot(roots).Execute(ctx, json.RawMessage(`{"path":`+quote(b)+`,"mode":"read"}`)); err != nil {
		t.Fatalf("授权失败: %v", err)
	}
	if _, err := fsys.Resolve(ctx, filesystem.Request{Path: filepath.Join(b, "x.txt"), Op: filesystem.OpRead}); err != nil {
		t.Fatalf("read 模式应可读: %v", err)
	}
	if _, err := fsys.Resolve(ctx, filesystem.Request{Path: filepath.Join(b, "x.txt"), Op: filesystem.OpWrite}); err == nil {
		t.Fatal("read 模式不应给写权限")
	}
}

// fail-closed：没装配根集合 / 相对路径 / 不存在的目录 / 不是目录 / 非法 mode。
func TestGrantRootFailClosed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := NewGrantRoot(nil).Execute(ctx, json.RawMessage(`{"path":`+quote(dir)+`}`)); err == nil {
		t.Fatal("未装配根集合应拒绝")
	}

	roots := filesystem.NewRootSet([]string{dir}, []string{dir})
	g := NewGrantRoot(roots)
	if _, err := g.Execute(ctx, json.RawMessage(`{"path":"relative/dir"}`)); err == nil {
		t.Fatal("相对路径应拒绝")
	}
	if _, err := g.Execute(ctx, json.RawMessage(`{"path":`+quote(filepath.Join(dir, "nope"))+`}`)); err == nil {
		t.Fatal("不存在的目录应拒绝")
	}
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Execute(ctx, json.RawMessage(`{"path":`+quote(file)+`}`)); err == nil {
		t.Fatal("文件不是目录，应拒绝")
	}
	if _, err := g.Execute(ctx, json.RawMessage(`{"path":`+quote(dir)+`,"mode":"rw"}`)); err == nil {
		t.Fatal("非法 mode 应拒绝")
	}
	if _, err := g.Execute(ctx, json.RawMessage(`{"path":""}`)); err == nil {
		t.Fatal("空路径应拒绝")
	}
}

// 重复授权不报错（幂等），回执说明"此前已授权"。
func TestGrantRootIdempotent(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	roots := filesystem.NewRootSet([]string{dir}, []string{dir})
	g := NewGrantRoot(roots)
	out, err := g.Execute(context.Background(), json.RawMessage(`{"path":`+quote(other)+`}`))
	if err != nil {
		t.Fatalf("首次授权: %v", err)
	}
	if !strings.Contains(out.(map[string]any)["note"].(string), "已授权") {
		t.Fatalf("首次回执 = %v", out)
	}
	out, err = g.Execute(context.Background(), json.RawMessage(`{"path":`+quote(other)+`}`))
	if err != nil {
		t.Fatalf("重复授权: %v", err)
	}
	if !strings.Contains(out.(map[string]any)["note"].(string), "此前已在授权范围内") {
		t.Fatalf("重复回执 = %v", out)
	}
}

// 指引文案：告诉模型"用户只是要求查看未授权目录时，不要自行申请"。
func TestGrantRootGuideline(t *testing.T) {
	roots := filesystem.NewRootSet([]string{t.TempDir()}, []string{t.TempDir()})
	g := NewGrantRoot(roots).Guideline()
	if g == "" {
		t.Fatal("指引不应为空")
	}
	for _, want := range []string{"明确授权", "不要自行申请"} {
		if !strings.Contains(g, want) {
			t.Fatalf("指引缺 %q: %s", want, g)
		}
	}
	// 没装配根集合 → 不贡献指引（没这个能力就别承诺）。
	if NewGrantRoot(nil).Guideline() != "" {
		t.Fatal("未装配根集合时不应贡献指引")
	}
}

// quote 给路径加引号（JSON 字符串）。
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
