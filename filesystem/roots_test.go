package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritableRootsDangerFull(t *testing.T) {
	p := &FileAccessPolicy{WorkspaceRoot: "/proj", JailReads: false, JailWrites: false}
	got := WritableRoots(p)
	if len(got) != 1 || got[0] != "/proj" {
		t.Errorf("DangerFull 应仅返回 [/proj]，got=%v", got)
	}
}

func TestWritableRootsJailed(t *testing.T) {
	p := &FileAccessPolicy{WorkspaceRoot: "/proj", JailReads: true, JailWrites: true}
	got := WritableRoots(p)
	// /proj + /tmp + os.TempDir()
	if len(got) != 3 {
		t.Errorf("应返回 3 项（workspace + /tmp + tempdir），got=%v", got)
	}
	abs, _ := filepath.Abs("/proj")
	if got[0] != abs {
		t.Errorf("workspace 应被 Abs 规范化，got=%q want=%q", got[0], abs)
	}
}

func TestWritableRootsDeduplicatesExtra(t *testing.T) {
	p := &FileAccessPolicy{
		WorkspaceRoot: "/proj",
		JailWrites:    true,
		WritableExtra: []string{"/proj", "/extra"},
	}
	got := WritableRoots(p)
	// /proj 出现两次应去重
	if len(got) != 4 { // /proj, /tmp, tempdir, /extra
		t.Errorf("应去重后 4 项，got=%v", got)
	}
}

func TestWritableRootsNormalizesTempDir(t *testing.T) {
	p := &FileAccessPolicy{WorkspaceRoot: "/proj", JailWrites: true}
	got := WritableRoots(p)
	abs, _ := filepath.Abs(os.TempDir())
	found := false
	for _, r := range got {
		if r == abs {
			found = true
		}
	}
	if !found {
		t.Errorf("应包含 Abs 化的 os.TempDir()=%q，got=%v", abs, got)
	}
}
