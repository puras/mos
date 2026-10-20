package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// 工具描述是模型唯一的使用指南（MoS 暂未提供“工具自带的系统提示贡献”通道），
// 所以分段读的正解必须留在描述里：调大 limit 一次读完，或一次发出多个 offset
// 的调用并行读完。描述被改写时这条会失败——防止指南被无意丢掉。
func TestReadFileDescriptionKeepsUsageGuide(t *testing.T) {
	desc := NewReadFile(nil).Description()
	for _, want := range []string{"limit", "offset", "并行", "size"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("read_file 描述应包含 %q（使用指南），实际：%s", want, desc)
		}
	}
}

// 默认上限 32KB：中等文件一次读完（往返少），不再一小段一小段地磨。
func TestReadFileDefaultLimitReadsMediumFileWhole(t *testing.T) {
	root, fsys := jail(t)
	body := strings.Repeat("x", 20000) // 20KB < 32KB
	if err := os.WriteFile(filepath.Join(root, "mid.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := NewReadFile(fsys).Execute(context.Background(), []byte(`{"path":"mid.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if len(m["content"].(string)) != 20000 {
		t.Fatalf("默认应一次读完 20000 字节, got %d", len(m["content"].(string)))
	}
	if m["next_offset"].(int) != 0 {
		t.Fatalf("读完时 next_offset 应为 0, got %v", m["next_offset"])
	}
}

// next_offset：读满 limit 时指向下一段起点；读完为 0（模型不用自己拿 size 去减）。
func TestReadFileNextOffset(t *testing.T) {
	root, fsys := jail(t)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Repeat("y", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	// 第一段：limit=400 → next_offset=400
	out, err := NewReadFile(fsys).Execute(context.Background(), []byte(`{"path":"big.txt","limit":400}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["next_offset"].(int) != 400 {
		t.Fatalf("next_offset = %v, want 400", m["next_offset"])
	}
	if len(m["content"].(string)) != 400 {
		t.Fatalf("content 长度 = %d, want 400", len(m["content"].(string)))
	}
	// 续读：用 next_offset 定位，读到最后一段
	out2, err := NewReadFile(fsys).Execute(context.Background(), []byte(`{"path":"big.txt","offset":400,"limit":400}`))
	if err != nil {
		t.Fatal(err)
	}
	m2 := out2.(map[string]any)
	if m2["next_offset"].(int) != 800 {
		t.Fatalf("next_offset = %v, want 800", m2["next_offset"])
	}
	out3, err := NewReadFile(fsys).Execute(context.Background(), []byte(`{"path":"big.txt","offset":800,"limit":400}`))
	if err != nil {
		t.Fatal(err)
	}
	m3 := out3.(map[string]any)
	if m3["next_offset"].(int) != 0 {
		t.Fatalf("读完时 next_offset 应为 0, got %v", m3["next_offset"])
	}
	if len(m3["content"].(string)) != 200 {
		t.Fatalf("末段长度 = %d, want 200", len(m3["content"].(string)))
	}
}
