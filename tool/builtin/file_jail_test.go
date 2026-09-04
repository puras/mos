package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	fs "github.com/puras/mos/filesystem"
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

func TestFindFilesRootEscapesDenied(t *testing.T) {
	dir := t.TempDir()
	args, _ := json.Marshal(map[string]any{"root": "/etc", "pattern": "*"})
	_, err := Glob{}.Execute(jailCtx(t, dir), args)
	var pd *fs.PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("应返回 *PathDeniedError，got=%v", err)
	}
}

func TestFindFilesDefaultsToWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	args, _ := json.Marshal(map[string]any{"pattern": "*.go"})
	r, err := Glob{}.Execute(jailCtx(t, dir), args)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	m := r.(map[string]any)
	if m["count"].(int) != 1 {
		t.Errorf("count=%v want=1（Root 空时应默认 WorkspaceRoot）", m["count"])
	}
}

func TestFindFilesNotConfiguredError(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"pattern": "*"})
	_, err := Glob{}.Execute(context.Background(), args)
	var nc *fs.NotConfiguredError
	if !errors.As(err, &nc) {
		t.Fatalf("未注入 fs 应返回 *fs.NotConfiguredError，got=%v", err)
	}
}

func TestGrepRootEscapesDenied(t *testing.T) {
	dir := t.TempDir()
	args, _ := json.Marshal(map[string]any{"root": "/etc", "pattern": "x"})
	_, err := Grep{}.Execute(jailCtx(t, dir), args)
	var pd *fs.PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("应返回 *PathDeniedError，got=%v", err)
	}
}

func TestGrepDefaultsToWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n// hello\n"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	args, _ := json.Marshal(map[string]any{"pattern": "hello"})
	r, err := Grep{}.Execute(jailCtx(t, dir), args)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	m := r.(map[string]any)
	if m["count"].(int) != 1 {
		t.Errorf("count=%v want=1（Root 空时应默认 WorkspaceRoot）", m["count"])
	}
}

func TestGrepNotConfiguredError(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"pattern": "x"})
	_, err := Grep{}.Execute(context.Background(), args)
	var nc *fs.NotConfiguredError
	if !errors.As(err, &nc) {
		t.Fatalf("未注入 fs 应返回 *fs.NotConfiguredError，got=%v", err)
	}
}

// TestWalkDoesNotFollowSymlinkedDirs 钉住 filepath.WalkDir 不跟随 symlink 的保证：
// workspace 内指向外部的 symlink 目录，其中的文件不得出现在 find_files / grep 结果里。
func TestWalkDoesNotFollowSymlinkedDirs(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(secret, []byte("package leak\n// needle\n"), 0o644); err != nil {
		t.Fatalf("准备外部文件失败: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "leak")); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}

	ctx := jailCtx(t, dir)

	fargs, _ := json.Marshal(map[string]any{"root": dir, "pattern": "*.go"})
	fr, err := Glob{}.Execute(ctx, fargs)
	if err != nil {
		t.Fatalf("FindFiles: %v", err)
	}
	if got := fr.(map[string]any)["count"].(int); got != 0 {
		t.Errorf("find_files 不应透过 symlink 目录枚举外部文件，got count=%d", got)
	}

	gargs, _ := json.Marshal(map[string]any{"root": dir, "pattern": "needle"})
	gr, err := Grep{}.Execute(ctx, gargs)
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if got := gr.(map[string]any)["count"].(int); got != 0 {
		t.Errorf("grep 不应透过 symlink 目录扫描外部文件，got count=%d", got)
	}
}
