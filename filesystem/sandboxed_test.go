package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"hegq.net/mos/agent/event"
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

// TestSandboxedModeJailWritesOnly：仅 jail 写（JailReads=false, JailWrites=true）。
// buildFileSystem 修 fail-open 后此组合可达——Sandboxed 实例不得自报 danger-full。
func TestSandboxedModeJailWritesOnly(t *testing.T) {
	s := &Sandboxed{Policy: &FileAccessPolicy{WorkspaceRoot: t.TempDir(), JailWrites: true}}
	if got := s.Mode(); got == ModeDangerFull {
		t.Errorf("Mode=%s：正在 jail 写的实例不应自报 danger-full", got)
	}
}

// recordingBus 记录 emit 的事件名与载荷（测试用）。
type recordingBus struct {
	names    []string
	payloads []any
}

func (b *recordingBus) Emit(_ context.Context, name string, payload any) error {
	b.names = append(b.names, name)
	b.payloads = append(b.payloads, payload)
	return nil
}

// TestSandboxedEmitsResolveEvents 验证 allow 与 deny 两条路径都发 fs/resolve 审计事件。
func TestSandboxedEmitsResolveEvents(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	fsys := &Sandboxed{Policy: p}
	bus := &recordingBus{}
	ctx := WithBus(context.Background(), bus)

	// denied
	if _, err := fsys.Resolve(ctx, Request{Path: "/etc/passwd", Op: OpRead, SessionID: "s"}); err == nil {
		t.Fatal("越界应被拒绝")
	}
	// allowed
	if _, err := fsys.Resolve(ctx, Request{Path: filepath.Join(dir, "x"), Op: OpRead, SessionID: "s"}); err != nil {
		t.Fatalf("workspace 内应通过: %v", err)
	}

	if len(bus.names) != 2 {
		t.Fatalf("应发 2 个事件，got=%d (%v)", len(bus.names), bus.names)
	}
	for i, n := range bus.names {
		if n != "fs/resolve" {
			t.Errorf("事件[%d] name=%q want=fs/resolve", i, n)
		}
	}
	d0, ok := bus.payloads[0].(event.FsResolveData)
	if !ok {
		t.Fatalf("载荷[0] 类型=%T want=event.FsResolveData", bus.payloads[0])
	}
	if d0.Decision != "denied" {
		t.Errorf("事件[0].decision=%q want=denied", d0.Decision)
	}
	if d0.Op != string(OpRead) || d0.Mode != string(ModeSaaSJailed) {
		t.Errorf("事件[0] op=%q mode=%q", d0.Op, d0.Mode)
	}
	d1 := bus.payloads[1].(event.FsResolveData)
	if d1.Decision != "allowed" {
		t.Errorf("事件[1].decision=%q want=allowed", d1.Decision)
	}
}

// TestSandboxedResolveWithoutBusIsSilent 验证未注入 bus 时不 panic（审计可选）。
func TestSandboxedResolveWithoutBusIsSilent(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	fsys := &Sandboxed{Policy: p}
	if _, err := fsys.Resolve(context.Background(), Request{Path: filepath.Join(dir, "x"), Op: OpRead}); err != nil {
		t.Fatalf("无 bus 时应正常工作: %v", err)
	}
}

// TestSandboxedNewFileThroughSymlinkDirDenied（PoC，预计 FAIL）：
// workspace 内的中间目录是越界 symlink，叶子文件不存在。
// realpathWrite 的"全新文件"快速通道只看 Lstat==NotExist 就放行，
// 但实际写入会沿中间 symlink 组件落到 workspace 外。
func TestSandboxedNewFileThroughSymlinkDirDenied(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}

	fsys := &Sandboxed{Policy: p}
	_, err := fsys.Resolve(context.Background(), Request{
		Path: filepath.Join(dir, "escape", "new.txt"), Op: OpWrite,
	})
	var pd *PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("经越界 symlink 目录写新文件应被拒绝，got=%v", err)
	}
}

// TestSandboxedDanglingChainThroughSymlinkAncestorDenied（PoC，预计 FAIL）：
// 更深一层——存在的前缀本身是越界 symlink，其后整段不存在。
// EvalSymlinks 对整条路径报 NotExist 时同样不能盲放行。
func TestSandboxedDanglingChainThroughSymlinkAncestorDenied(t *testing.T) {
	p, dir := newJailedPolicy(t, ModeSaaSJailed)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("不支持 symlink: %v", err)
	}

	fsys := &Sandboxed{Policy: p}
	_, err := fsys.Resolve(context.Background(), Request{
		Path: filepath.Join(dir, "escape", "deep", "new.txt"), Op: OpWrite,
	})
	var pd *PathDeniedError
	if !errors.As(err, &pd) {
		t.Fatalf("越界 symlink 后的悬空链写新文件应被拒绝，got=%v", err)
	}
}
