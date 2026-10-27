package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/puras/mos/filesystem"
)

// newObservedTools 构造一组带观察门的文件工具 + jail。
func newObservedTools(t *testing.T, root string) (rd *Read, wr *Write, ed *Edit) {
	t.Helper()
	fsys := filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
		ReadRoots:  []string{root},
		WriteRoots: []string{root},
	})
	gate := NewObserveGate()
	return NewReadObserved(fsys, gate), NewWriteObserved(fsys, gate), NewEditObserved(fsys, gate)
}

// 正常流程：读 → 改 → 写 全链路不受影响；新建文件不需要先读。
func TestObserveHappyPath(t *testing.T) {
	root := t.TempDir()
	rd, wr, ed := newObservedTools(t, root)

	// 新文件：没读过也不存在 → createIfAbsent → 直接可写
	if _, err := wr.Execute(context.Background(), []byte(`{"path":"new.txt","content":"v1"}`)); err != nil {
		t.Fatalf("新建文件不该被拦: %v", err)
	}

	// 读 → 改：观察门放行（读后没变）
	if _, err := rd.Execute(context.Background(), []byte(`{"path":"new.txt"}`)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := ed.Execute(context.Background(), []byte(
		`{"path":"new.txt","edits":[{"old_string":"v1","new_string":"v2"}]}`)); err != nil {
		t.Fatalf("读后编辑应放行: %v", err)
	}
}

// 核心契约：没读过的已有文件不许覆盖（DSH 的 FS_NOT_OBSERVED）。
func TestObserveWriteRequiresRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(path, []byte("别人的内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, wr, _ := newObservedTools(t, root)

	_, err := wr.Execute(context.Background(), []byte(`{"path":"existing.txt","content":"模型的内容"}`))
	if err == nil {
		t.Fatal("覆盖未读过的文件应被拒")
	}
	if !strings.Contains(err.Error(), "已存在但未被读取过") {
		t.Fatalf("理由应可指导模型: %v", err)
	}
	// 拒绝后文件不能被改
	b, _ := os.ReadFile(path)
	if string(b) != "别人的内容" {
		t.Fatalf("文件被改了: %s", b)
	}
}

// 读后被外部修改 → 写/改被拒（丢失更新防护）；重读后放行。
func TestObserveStaleAfterExternalChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	os.WriteFile(path, []byte("v1"), 0o644)
	rd, wr, ed := newObservedTools(t, root)

	if _, err := rd.Execute(context.Background(), []byte(`{"path":"f.txt"}`)); err != nil {
		t.Fatal(err)
	}

	// 外部修改（shell 或人改的——观察门不知道，但 CAS 会抓住）
	os.WriteFile(path, []byte("被别人改过的内容"), 0o644)

	_, err := ed.Execute(context.Background(), []byte(
		`{"path":"f.txt","edits":[{"old_string":"v1","new_string":"v2"}]}`))
	if err == nil || !strings.Contains(err.Error(), "自读取后已被修改") {
		t.Fatalf("读后被改应拒绝编辑: %v", err)
	}

	_, err = wr.Execute(context.Background(), []byte(`{"path":"f.txt","content":"模型的内容"}`))
	if err == nil || !strings.Contains(err.Error(), "自读取后已被修改") {
		t.Fatalf("读后被改应拒绝写入: %v", err)
	}

	// 重读 → 看到真实状态 → 再写成功（自我纠正回路）
	if _, err := rd.Execute(context.Background(), []byte(`{"path":"f.txt"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := wr.Execute(context.Background(), []byte(`{"path":"f.txt","content":"v2"}`)); err != nil {
		t.Fatalf("重读后应放行: %v", err)
	}
}

// edit 前必须先读；读到"不存在"后可以直接创建。
func TestObserveEditRequiresRead(t *testing.T) {
	root := t.TempDir()
	rd, _, ed := newObservedTools(t, root)

	if _, err := ed.Execute(context.Background(), []byte(
		`{"path":"a.txt","edits":[{"old_string":"x","new_string":"y"}]}`)); err == nil {
		t.Fatal("没读过的文件应拒绝编辑")
	} else if !strings.Contains(err.Error(), "必须先 read") {
		t.Fatalf("理由应可指导模型: %v", err)
	}

	// 确认不存在 → 记 absent → edit 报"不存在"，write 可创建
	if _, err := rd.Execute(context.Background(), []byte(`{"path":"a.txt"}`)); err == nil {
		t.Fatal("读不存在的文件应报错")
	}
	if _, err := ed.Execute(context.Background(), []byte(
		`{"path":"a.txt","edits":[{"old_string":"x","new_string":"y"}]}`)); err == nil {
		t.Fatal("观察为不存在的文件应拒绝编辑")
	} else if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("理由应说明不存在: %v", err)
	}
}

// 读后被删除 → 写被拒（stale），重读（确认不存在）后可重建。
func TestObserveDeletedAfterRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	os.WriteFile(path, []byte("v1"), 0o644)
	rd, wr, _ := newObservedTools(t, root)

	if _, err := rd.Execute(context.Background(), []byte(`{"path":"f.txt"}`)); err != nil {
		t.Fatal(err)
	}
	os.Remove(path)

	_, err := wr.Execute(context.Background(), []byte(`{"path":"f.txt","content":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "自读取后已被删除") {
		t.Fatalf("读后被删应拒绝写入: %v", err)
	}

	// 重读确认不存在 → 可重建（createIfAbsent）
	if _, err := rd.Execute(context.Background(), []byte(`{"path":"f.txt"}`)); err == nil {
		t.Fatal("应报不存在")
	}
	if _, err := wr.Execute(context.Background(), []byte(`{"path":"f.txt","content":"x"}`)); err != nil {
		t.Fatalf("确认不存在后应可创建: %v", err)
	}
}

// 连续写：write 成功后记录新版本，同参数再写应放行（自己刚写的不是别人的改动）。
func TestObserveConsecutiveWrites(t *testing.T) {
	root := t.TempDir()
	_, wr, _ := newObservedTools(t, root)

	if _, err := wr.Execute(context.Background(), []byte(`{"path":"f.txt","content":"v1"}`)); err != nil {
		t.Fatal(err)
	}
	// 第二次写（没有再读）：记录的版本就是自己刚写的版本 → 放行
	if _, err := wr.Execute(context.Background(), []byte(`{"path":"f.txt","content":"v2"}`)); err != nil {
		t.Fatalf("连续写应放行: %v", err)
	}
}

// nil 门 = 旧行为（无条件变更）。
func TestObserveNilGateIsLegacy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "existing.txt")
	os.WriteFile(path, []byte("旧内容"), 0o644)
	fsys := filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
		ReadRoots: []string{root}, WriteRoots: []string{root},
	})
	wr := NewWrite(fsys) // 无门
	if _, err := wr.Execute(context.Background(), []byte(`{"path":"existing.txt","content":"覆盖"}`)); err != nil {
		t.Fatalf("nil 门 = 旧行为: %v", err)
	}
}

// read 记录的观察带路径维度：不同文件互不干扰。
func TestObservePerFile(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	os.WriteFile(a, []byte("A"), 0o644)
	rd, wr, _ := newObservedTools(t, root)

	if _, err := rd.Execute(context.Background(), []byte(`{"path":"a.txt"}`)); err != nil {
		t.Fatal(err)
	}
	// 写另一个没读过的文件 → 拒；写读过的 → 放行
	if _, err := wr.Execute(context.Background(), []byte(`{"path":"b.txt","content":"B"}`)); err != nil {
		t.Fatalf("新文件 b 应可创建: %v", err)
	}
	if _, err := wr.Execute(context.Background(), []byte(`{"path":"a.txt","content":"A2"}`)); err != nil {
		t.Fatalf("读过的 a 应可覆盖: %v", err)
	}
}
