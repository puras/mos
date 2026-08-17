package fs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDangerFullResolvesAbsolute(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	os.WriteFile(file, []byte("x"), 0o644)

	fsys := &Local{Mode: ModeDangerFull}
	r, err := fsys.Resolve(context.Background(), Request{Path: file, Op: OpRead})
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if r.DisplayPath != file {
		t.Errorf("DisplayPath=%q want=%q", r.DisplayPath, file)
	}
	if r.TargetKey != file {
		t.Errorf("TargetKey=%q want=%q", r.TargetKey, file)
	}
}

func TestLocalDangerFullAllowsEscape(t *testing.T) {
	// DangerFull 模式不做 jail；越界路径应被透传
	fsys := &Local{Mode: ModeDangerFull}
	r, err := fsys.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpRead})
	if err != nil {
		t.Fatalf("DangerFull 应允许越界: %v", err)
	}
	if r.TargetKey != "/etc/passwd" {
		t.Errorf("TargetKey=%q want=/etc/passwd", r.TargetKey)
	}
}
