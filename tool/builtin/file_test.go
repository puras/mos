package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/puras/mos/filesystem"
)

func jail(t *testing.T) (string, filesystem.FileSystem) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return root, filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
		ReadRoots:  []string{root},
		WriteRoots: []string{root},
	})
}

// 正常读：内容回填。
func TestReadFileInsideJail(t *testing.T) {
	root, fsys := jail(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewReadFile(fsys).Execute(context.Background(), []byte(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatalf("读取 jail 内文件: %v", err)
	}
	m := out.(map[string]any)
	if m["content"] != "hello" {
		t.Fatalf("content = %v", m["content"])
	}
}

// 越界读：返回 *PathDeniedError，且拒绝理由可回填给模型。
func TestReadFileOutsideJailDenied(t *testing.T) {
	_, fsys := jail(t)
	_, err := NewReadFile(fsys).Execute(context.Background(), []byte(`{"path":"../../etc/passwd"}`))
	if err == nil {
		t.Fatal("越界读应被拒绝")
	}
	var denied *filesystem.PathDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("应返回 *PathDeniedError，得到 %T: %v", err, err)
	}
}

// 未装配文件能力：fail-closed 拒绝，不放行。
func TestReadFileWithoutFilesystemFailsClosed(t *testing.T) {
	_, err := NewReadFile(nil).Execute(context.Background(), []byte(`{"path":"a.txt"}`))
	if err == nil {
		t.Fatal("未装配文件能力应拒绝")
	}
	var notCfg *filesystem.NotConfiguredError
	if !errors.As(err, &notCfg) {
		t.Fatalf("应返回 *NotConfiguredError，得到 %T: %v", err, err)
	}
}

// 正常写：落盘。
func TestWriteFileInsideJail(t *testing.T) {
	root, fsys := jail(t)
	out, err := NewWriteFile(fsys).Execute(context.Background(),
		[]byte(`{"path":"sub/b.txt","content":"data"}`))
	if err != nil {
		t.Fatalf("写入 jail 内文件: %v", err)
	}
	if out.(map[string]any)["bytes"].(int) != 4 {
		t.Fatalf("out = %v", out)
	}
	got, err := os.ReadFile(filepath.Join(root, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Fatalf("落盘内容 = %q", got)
	}
}

// 越界写：拒绝且**不落盘**（不能只看错误，还得看副作用没发生）。
func TestWriteFileOutsideJailDenied(t *testing.T) {
	root, fsys := jail(t)
	outside := filepath.Join(t.TempDir(), "evil.txt")
	_, err := NewWriteFile(fsys).Execute(context.Background(),
		[]byte(`{"path":"`+outside+`","content":"pwn"}`))
	if err == nil {
		t.Fatal("越界写应被拒绝")
	}
	var denied *filesystem.PathDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("应返回 *PathDeniedError，得到 %T: %v", err, err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("被拒的写入不应落盘")
	}
	_ = root
}

// 经越界 symlink 写：拒绝且不落盘。
func TestWriteFileThroughSymlinkDenied(t *testing.T) {
	root, fsys := jail(t)
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(root, "escape")); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}
	_, err := NewWriteFile(fsys).Execute(context.Background(),
		[]byte(`{"path":"escape/new.txt","content":"pwn"}`))
	if err == nil {
		t.Fatal("经越界 symlink 写应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("被拒的写入不应落到 jail 外")
	}
}

// FileTools 返回读 + 写两个工具。
func TestFileTools(t *testing.T) {
	_, fsys := jail(t)
	tools := FileTools(fsys)
	if len(tools) != 2 {
		t.Fatalf("工具数 = %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name()] = true
	}
	if !names["read_file"] || !names["write_file"] {
		t.Fatalf("工具名 = %v", names)
	}
}
