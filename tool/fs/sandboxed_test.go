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
