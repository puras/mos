package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newJailedPolicy(t *testing.T, mode Mode) (*FileAccessPolicy, string) {
	t.Helper()
	dir := t.TempDir()
	return &FileAccessPolicy{WorkspaceRoot: dir, JailReads: true, JailWrites: true}, dir
}

func TestSandboxedReadInsideJailPasses(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	file := filepath.Join(dir, "a.txt")
	os.WriteFile(file, []byte("x"), 0o644)

	fsys := &Sandboxed{Policy: p}
	r, err := fsys.Resolve(context.Background(), Request{Path: file, Op: OpRead})
	if err != nil {
		t.Fatalf("应通过: %v", err)
	}
	if r.TargetKey != file {
		t.Errorf("TargetKey=%q want=%q", r.TargetKey, file)
	}
}

func TestSandboxedReadRelativeInsideJailPasses(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y"), 0o644)

	fsys := &Sandboxed{Policy: p}
	r, err := fsys.Resolve(context.Background(), Request{Path: "b.txt", Op: OpRead})
	if err != nil {
		t.Fatalf("应通过相对路径: %v", err)
	}
	want := filepath.Join(dir, "b.txt")
	if r.TargetKey != want {
		t.Errorf("TargetKey=%q want=%q", r.TargetKey, want)
	}
}

func TestSandboxedReadEscapeDenied(t *testing.T) {
	p, _ := newJailedPolicy(t, ModeSaaSJailed)
	fsys := &Sandboxed{Policy: p}
	_, err := fsys.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpRead})
	var pd *PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("应返回 *PathDeniedError，got=%v", err)
	}
	if pd.Op != OpRead {
		t.Errorf("Op=%q want=read", pd.Op)
	}
}

func TestSandboxedReadEscapeWithDotDotDenied(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	fsys := &Sandboxed{Policy: p}
	_, err := fsys.Resolve(context.Background(), Request{Path: filepath.Join(dir, "../etc/passwd"), Op: OpRead})
	var pd *PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("应返回 *PathDeniedError，got=%v", err)
	}
}

func TestSandboxedWriteEscapeDenied(t *testing.T) {
	p, _ := newJailedPolicy(t, ModeSaaSJailed)
	fsys := &Sandboxed{Policy: p}
	_, err := fsys.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpWrite})
	var pd *PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("Op=Write 应返回 *PathDeniedError，got=%v", err)
	}
}

func TestSandboxedWriteInsideWorkspacePasses(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeLocalJailed)
	target := filepath.Join(dir, "out.txt")
	fsys := &Sandboxed{Policy: p}
	r, err := fsys.Resolve(context.Background(), Request{Path: target, Op: OpWrite})
	if err != nil {
		t.Fatalf("应通过: %v", err)
	}
	if r.TargetKey != target {
		t.Errorf("TargetKey=%q want=%q", r.TargetKey, target)
	}
}

func TestSandboxedSymlinkEscapeDenied(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	// 在 workspace 外建一个文件，并在 workspace 内放一个 symlink 指向它
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	os.WriteFile(target, []byte("s"), 0o644)
	link := filepath.Join(dir, "leak.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}

	fsys := &Sandboxed{Policy: p}
	_, err := fsys.Resolve(context.Background(), Request{Path: link, Op: OpRead})
	// Op=Read 走 Lexical 即返回（DSH read 透传语义）；此用例仅验证 Op=Write 时拒绝
	if err == nil {
		t.Log("Op=Read 透传 symlink（DSH 一致）")
	}
	_, err = fsys.Resolve(context.Background(), Request{Path: link, Op: OpWrite})
	var pd *PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("Op=Write 通过 symlink 越界应被拒绝，got=%v", err)
	}
}

// TestSandboxedBrokenSymlinkEscapeDenied：broken symlink 指向 workspace 外不存在的目标。
// 写操作会沿 symlink 在外部创建文件——必须拒绝，不能因 EvalSymlinks 返回 IsNotExist 而放行。
func TestSandboxedBrokenSymlinkEscapeDenied(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	outside := t.TempDir()
	link := filepath.Join(dir, "leak.txt")
	missingTarget := filepath.Join(outside, "does-not-exist.txt")
	if err := os.Symlink(missingTarget, link); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}

	fsys := &Sandboxed{Policy: p}
	_, err := fsys.Resolve(context.Background(), Request{Path: link, Op: OpWrite})
	var pd *PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("broken symlink 越界应被拒绝（写入会在 workspace 外创建文件），got=%v", err)
	}
}

// TestSandboxedSymlinkInsideWorkspacePasses：symlink 在 workspace 内指向 workspace 内的目标（合法）。
func TestSandboxedSymlinkInsideWorkspacePasses(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	real := filepath.Join(dir, "real.txt")
	os.WriteFile(real, []byte("x"), 0o644)
	link := filepath.Join(dir, "alias.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}

	fsys := &Sandboxed{Policy: p}
	_, err := fsys.Resolve(context.Background(), Request{Path: link, Op: OpWrite})
	if err != nil {
		t.Fatalf("workspace 内 symlink → workspace 内目标应允许 Op=Write: %v", err)
	}
}
