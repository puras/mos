package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ws 造一个 jail 根，返回根路径。
func ws(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// macOS 的 /tmp 是 symlink（/tmp → /private/tmp），先解析成实体路径，
	// 免得测试断言被环境差异干扰。
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newFS 造一个读写都锁在 root 内的策略（宿主最常见的配法）。
func newFS(root string) *Sandboxed {
	return NewSandboxed(&FileAccessPolicy{ReadRoots: []string{root}, WriteRoots: []string{root}})
}

// 根内的普通文件：读写都放行。
func TestResolveInsideWorkspace(t *testing.T) {
	root := ws(t)
	mustWrite(t, filepath.Join(root, "a.txt"), "hello")
	fs := newFS(root)
	ctx := context.Background()

	for _, op := range []Op{OpRead, OpWrite} {
		res, err := fs.Resolve(ctx, Request{Path: "a.txt", Op: op})
		if err != nil {
			t.Fatalf("%s 应放行: %v", op, err)
		}
		if res.TargetKey != filepath.Join(root, "a.txt") {
			t.Fatalf("%s TargetKey = %q", op, res.TargetKey)
		}
	}
}

// 相对路径以 jail 根为锚点。
func TestResolveRelativeAnchoredAtWorkspace(t *testing.T) {
	root := ws(t)
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), "x")
	res, err := newFS(root).Resolve(context.Background(), Request{Path: "sub/b.txt", Op: OpRead})
	if err != nil {
		t.Fatalf("相对路径应放行: %v", err)
	}
	if !strings.HasPrefix(res.TargetKey, root) {
		t.Fatalf("TargetKey = %q，应在 jail 根 %q 内", res.TargetKey, root)
	}
}

// 词法越界：.. 逃逸与绝对路径逃逸都拒绝。
func TestResolveRejectsLexicalEscape(t *testing.T) {
	root := ws(t)
	fs := newFS(root)
	ctx := context.Background()

	cases := []string{
		"../outside.txt",
		"../../etc/passwd",
		"/etc/passwd",
		"sub/../../outside.txt",
	}
	for _, p := range cases {
		_, err := fs.Resolve(ctx, Request{Path: p, Op: OpRead})
		if err == nil {
			t.Fatalf("%q 应被拒绝", p)
		}
		var denied *PathDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("%q 应返回 *PathDeniedError，得到 %T: %v", p, err, err)
		}
	}
}

// 实体越界（读也要拦）：workspace 内指向 jail 外的 symlink，读它必须拒绝。
// 归档实现只对写做 Realpath，这条是重建版补上的洞。
func TestResolveRejectsSymlinkEscapeOnRead(t *testing.T) {
	root := ws(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, outside, "TOP SECRET")
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}
	_, err := newFS(root).Resolve(context.Background(), Request{Path: "link.txt", Op: OpRead})
	if err == nil {
		t.Fatal("经 symlink 读 jail 外文件应被拒绝")
	}
	var denied *PathDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("应返回 *PathDeniedError，得到 %T", err)
	}
}

// 实体越界（写）：目标不存在但中间组件是越界 symlink（ws/escape → /etc）。
// 叶子 Lstat 报 NotExist，若只做词法校验就会放行，实际写入落到 /etc。
func TestResolveRejectsSymlinkComponentOnCreate(t *testing.T) {
	root := ws(t)
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(root, "escape")); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}
	_, err := newFS(root).Resolve(context.Background(), Request{Path: "escape/new.txt", Op: OpWrite})
	if err == nil {
		t.Fatal("经越界 symlink 组件创建文件应被拒绝")
	}
}

// 目标本身是 symlink 且指向 jail 外：写也应拒绝。
func TestResolveRejectsSymlinkTargetOnWrite(t *testing.T) {
	root := ws(t)
	outside := filepath.Join(t.TempDir(), "target.txt")
	mustWrite(t, outside, "x")
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}
	_, err := newFS(root).Resolve(context.Background(), Request{Path: "link.txt", Op: OpWrite})
	if err == nil {
		t.Fatal("写 jail 外的 symlink 目标应被拒绝")
	}
}

// symlink 环：不得死循环，提前拒绝。
func TestResolveRejectsSymlinkLoop(t *testing.T) {
	root := ws(t)
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}
	if _, err := newFS(root).Resolve(context.Background(), Request{Path: "a", Op: OpRead}); err == nil {
		t.Fatal("symlink 环应被拒绝")
	}
}

// 未声明允许的根 = 拒绝一切（fail-closed），不是"等于放开"。
func TestResolveWithoutRootsDeniesAll(t *testing.T) {
	root := ws(t)
	fs := NewSandboxed(&FileAccessPolicy{})
	if _, err := fs.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpRead}); err == nil {
		t.Fatal("未声明根应拒绝一切")
	}
	// 策略存在但某个操作没有根：该操作拒绝，另一操作不受影响
	fs = NewSandboxed(&FileAccessPolicy{ReadRoots: []string{root}})
	if _, err := fs.Resolve(context.Background(), Request{Path: "a.txt", Op: OpRead}); err != nil {
		t.Fatalf("有读根时读应放行: %v", err)
	}
	if _, err := fs.Resolve(context.Background(), Request{Path: "a.txt", Op: OpWrite}); err == nil {
		t.Fatal("无写根时写应拒绝")
	}
	fs = NewSandboxed(nil)
	if _, err := fs.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpRead}); err == nil {
		t.Fatal("nil 策略应拒绝一切")
	}
}

// 多根：宿主按自己的形态声明可读/可写根（框架不预设形态）。
func TestHostDeclaredRoots(t *testing.T) {
	root := ws(t)
	extra := ws(t)
	fs := NewSandboxed(&FileAccessPolicy{ReadRoots: []string{root}, WriteRoots: []string{root, extra}})
	ctx := context.Background()

	// jail 根与额外根之外的绝对路径：写也拒绝
	outsideAbs := filepath.Join(t.TempDir(), "x.txt")
	if _, err := fs.Resolve(ctx, Request{Path: outsideAbs, Op: OpWrite}); err == nil {
		t.Fatal("jail 根外未声明的路径应拒绝写")
	}
	res, err := fs.Resolve(ctx, Request{Path: filepath.Join(extra, "x.txt"), Op: OpWrite})
	if err != nil {
		t.Fatalf("显式声明的可写根应放行: %v", err)
	}
	if res.TargetKey == "" {
		t.Fatal("TargetKey 不应为空")
	}
	if _, err := fs.Resolve(ctx, Request{Path: filepath.Join(extra, "x.txt"), Op: OpRead}); err == nil {
		t.Fatal("额外可写根不自动获得读权限")
	}
}

// 根集合：只有宿主显式声明的根，没有隐式的 /tmp。
func TestRootsNoImplicitTmp(t *testing.T) {
	p := &FileAccessPolicy{ReadRoots: []string{"/ws"}, WriteRoots: []string{"/ws", "/extra"}}
	if got := p.RootsFor(OpWrite); len(got) != 2 {
		t.Fatalf("写根 = %v，应只有显式声明的两个", got)
	}
	for _, r := range p.RootsFor(OpWrite) {
		if r == "/tmp" || r == os.TempDir() {
			t.Fatalf("不应隐式包含共享临时目录: %v", p.RootsFor(OpWrite))
		}
	}
	if got := p.RootsFor(OpRead); len(got) != 1 {
		t.Fatalf("读根 = %v", got)
	}
}

// 「完全开放」是宿主显式声明的规则，不是内置模式：WriteRoots: ["/"] 即放开全部。
func TestHostCanDeclareOpenRoots(t *testing.T) {
	root := ws(t)
	mustWrite(t, filepath.Join(root, "a.txt"), "x")
	fs := NewSandboxed(&FileAccessPolicy{ReadRoots: []string{"/"}, WriteRoots: []string{"/"}})
	if _, err := fs.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpRead}); err != nil {
		t.Fatalf("宿主显式声明 / 为读根时应放行: %v", err)
	}
}
