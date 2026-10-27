//go:build darwin

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// 端到端：运行期加根 → **内核强制**跟着放行。
//
// 这是"运行期添加目录"在进程层的真实证明：不是配置看起来变了，而是被隔离
// 的进程真的能写新根——且在加根之前确实写不了（Seatbelt profile 每次调用
// 现生成，所以放行边界是跟着动态范围走的）。
func TestDynamicScopeWriteFollowsRuntimeRoot(t *testing.T) {
	if _, err := exec.LookPath(seatbeltBin); err != nil {
		t.Skipf("sandbox-exec 不可用: %v", err)
	}
	a := t.TempDir()
	b := t.TempDir()
	target := filepath.Join(b, "out.txt")

	var mu sync.RWMutex
	roots := []string{a}
	s := &Local{
		WorkDir: a,
		Scope: ScopeFunc(
			func() []string { mu.RLock(); defer mu.RUnlock(); return append([]string(nil), roots...) },
			func() []string { mu.RLock(); defer mu.RUnlock(); return append([]string(nil), roots...) },
		),
	}

	// 加根前：写 B 应被内核拒绝（命令跑起来了，但写失败 → 非零退出码）。
	res, err := s.Run(context.Background(), Request{Command: "touch " + target})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("加根前写 %s 应被拒绝，实际成功: %s", target, res.Output)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("加根前不该写出文件")
	}

	// 运行期加根（宿主授权 B）→ 立刻可写。
	mu.Lock()
	roots = append(roots, b)
	mu.Unlock()

	res, err = s.Run(context.Background(), Request{Command: "touch " + target})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("加根后写 %s 应放行，退出码 %d: %s", target, res.ExitCode, res.Output)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("加根后文件应存在: %v", err)
	}
}
