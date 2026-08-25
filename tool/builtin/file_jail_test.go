package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"hegq.net/mos/tool/fs"
)

// jailCtx 返回注入了 workspace jail 的 ctx（读写都 jail）。
func jailCtx(t *testing.T, dir string) context.Context {
	t.Helper()
	fsys := &fs.Sandboxed{Policy: &fs.FileAccessPolicy{
		WorkspaceRoot: dir, JailReads: true, JailWrites: true,
	}}
	return fs.WithFileSystem(context.Background(), fsys)
}

func TestReadFileUsesFsContext(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	args, _ := json.Marshal(map[string]any{"path": file})
	r, err := ReadFile{}.Execute(jailCtx(t, dir), args)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	m := r.(map[string]any)
	if m["content"] != "hello" {
		t.Errorf("content=%v want=hello", m["content"])
	}
}

func TestReadFileEscapeDenied(t *testing.T) {
	dir := t.TempDir()
	args, _ := json.Marshal(map[string]any{"path": "/etc/passwd"})
	_, err := ReadFile{}.Execute(jailCtx(t, dir), args)
	var pd *fs.PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("应返回 *PathDeniedError，got=%v", err)
	}
}

func TestReadFileNotConfiguredError(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"path": "x.txt"})
	_, err := ReadFile{}.Execute(context.Background(), args)
	var nc *fs.NotConfiguredError
	if !errors.As(err, &nc) {
		t.Fatalf("未注入 fs 应返回 *NotConfiguredError，got=%v", err)
	}
}

func TestWriteFileEscapeDenied(t *testing.T) {
	dir := t.TempDir()
	args, _ := json.Marshal(map[string]any{"path": "/etc/passwd", "content": "x"})
	_, err := WriteFile{}.Execute(jailCtx(t, dir), args)
	var pd *fs.PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("应返回 *PathDeniedError，got=%v", err)
	}
}

func TestWriteFileInsideWorkspacePasses(t *testing.T) {
	dir := t.TempDir()
	args, _ := json.Marshal(map[string]any{"path": filepath.Join(dir, "out.txt"), "content": "y"})
	_, err := WriteFile{}.Execute(jailCtx(t, dir), args)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Errorf("文件未创建: %v", err)
	}
}

func TestWriteFileNotConfiguredError(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"path": "x.txt", "content": "x"})
	_, err := WriteFile{}.Execute(context.Background(), args)
	var nc *fs.NotConfiguredError
	if !errors.As(err, &nc) {
		t.Fatalf("未注入 fs 应返回 *fs.NotConfiguredError，got=%v", err)
	}
}
