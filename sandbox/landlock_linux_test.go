//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Landlock 后端：写范围是**内核强制**的（不是我们自己在 Go 里判断路径）。
//
// 这些用例跑在真实的内核 LSM 上：命令由「重新执行出来的自己」跑，
// 约束在 exec 之前由内核套上。
func TestLandlockEnforcesWriteScope(t *testing.T) {
	if landlockABIVersion() < 1 {
		t.Skip("本机内核没有 Landlock（Linux < 5.13 或未启用 CONFIG_SECURITY_LANDLOCK）")
	}
	work := t.TempDir()
	outside := t.TempDir()
	s := &Local{WorkDir: work}
	ctx := context.Background()

	// 写根外：内核拒绝。
	res, err := s.Run(ctx, Request{Command: "touch " + filepath.Join(outside, "x")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("写工作区外应被拒绝: out=%s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Fatal("写工作区外竟然写成了")
	}

	// 写根内：成功。
	res, err = s.Run(ctx, Request{Command: "touch ./inside && echo ok"})
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "ok") {
		t.Fatalf("写工作区内应成功: err=%v exit=%d out=%s", err, res.ExitCode, res.Output)
	}

	// 写根外建目录：内核拒绝（MAKE_DIR 也在写范围里）。
	res, err = s.Run(ctx, Request{Command: "mkdir -p " + filepath.Join(outside, "d")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("在工作区外建目录应被拒绝: out=%s", res.Output)
	}

	// 读工作区外：允许——语义是「只收窄写」，不是「只收窄读写」。
	outsideFile := filepath.Join(outside, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = s.Run(ctx, Request{Command: "cat " + outsideFile})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("读工作区外应被允许（只收窄写）: out=%s", res.Output)
	}
}

// Landlock 后端：/dev/null 必须可写。
//
// shell 的通用丢弃口（`2>/dev/null`）——不放行它，一大批正常命令会直接
// 失败（连 SDK 自己的 ulimit 前缀都跑不了）。
func TestLandlockAllowsDevNull(t *testing.T) {
	if landlockABIVersion() < 1 {
		t.Skip("本机内核没有 Landlock")
	}
	s := &Local{WorkDir: t.TempDir()}
	res, err := s.Run(context.Background(), Request{Command: "echo hi > /dev/null && echo ok"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "ok") {
		t.Fatalf("写 /dev/null 应被允许: exit=%d out=%s", res.ExitCode, res.Output)
	}
}

// Landlock 后端：写根内部的跨目录重命名（refer 权限）应当能用。
//
// 内核 ≥5.19（ABI V2）才能处理 refer；更老的内核上这一步会被拒——
// 那不是 bug，是已知缺口（见 landlock_linux.go 的注释）。
func TestLandlockAllowsRenameInsideRoot(t *testing.T) {
	if landlockABIVersion() < 2 {
		t.Skip("内核 ABI < 2（refer 不可用），跨目录重命名本就被拒")
	}
	work := t.TempDir()
	s := &Local{WorkDir: work}
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := s.Run(ctx, Request{Command: "echo x > a.txt && mv a.txt sub/b.txt && echo ok"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "ok") {
		t.Fatalf("工作区内重命名应被允许: exit=%d out=%s", res.ExitCode, res.Output)
	}
}

// Landlock 后端：内核没有 Landlock 时的 fail-closed 路径。
//
// 说明：本机内核有 Landlock，所以「内核不支持」这一支**没法在这台机器上
// 真实跑出来**（改内核不是测试能做的事）。这里只验证探测函数与分支形状：
// 探测返回 0 时必须走 fail-closed 分支，而不是放行。
//
// 注意：不能在测试进程里直接调 applyLandlock——它会**把测试进程自己**
// 套进约束（Landlock 限制的是调用它的线程），后续 t.TempDir 清理会被拒。
func TestLandlockFailsClosedWithoutKernelSupport(t *testing.T) {
	abi := landlockABIVersion()
	if abi >= 1 {
		t.Skipf("本机内核支持 Landlock（ABI v%d），fail-closed 分支跑不到", abi)
	}
	if err := applyLandlock([]string{os.TempDir()}); err == nil {
		t.Fatal("内核没有 Landlock 时套约束必须报错（fail-closed）")
	}
}
