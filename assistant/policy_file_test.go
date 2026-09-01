package assistant

import (
	"path/filepath"
	"testing"

	fs "hegq.net/mos/filesystem"
)

func TestWithinDirAllowsExtraWritable(t *testing.T) {
	extra := t.TempDir()
	p := &fs.FileAccessPolicy{WorkspaceRoot: t.TempDir(), JailWrites: true, WritableExtra: []string{extra}}
	// 落在 WritableExtra 内应通过
	if err := withinDir(p, filepath.Join(extra, "x")); err != nil {
		t.Errorf("WritableExtra 内应通过: %v", err)
	}
}

func TestWithinDirDeniesOutsideAllRoots(t *testing.T) {
	p := &fs.FileAccessPolicy{WorkspaceRoot: t.TempDir(), JailWrites: true}
	if err := withinDir(p, "/etc/passwd"); err == nil {
		t.Errorf("应拒绝越界")
	}
}
