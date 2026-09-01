package sandbox

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalRunBasic(t *testing.T) {
	out, err := (&Local{}).Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Fatalf("out = %q, want hello", out)
	}
}

func TestLocalEmptyCommand(t *testing.T) {
	if _, err := (&Local{}).Run(context.Background(), "   "); err == nil {
		t.Fatal("empty command should error")
	}
}

// 工作目录 jail：命令在 WorkDir 内执行。
func TestLocalWorkDirJail(t *testing.T) {
	dir := t.TempDir()
	out, err := (&Local{WorkDir: dir}).Run(context.Background(), "pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if strings.TrimSpace(out) != want {
		t.Fatalf("pwd = %q, want %q", strings.TrimSpace(out), want)
	}
}

func TestLocalInvalidWorkDir(t *testing.T) {
	sb := &Local{WorkDir: filepath.Join(t.TempDir(), "not-exist")}
	if _, err := sb.Run(context.Background(), "pwd"); err == nil {
		t.Fatal("invalid workdir should error")
	}
}

// 单次调用超时：超时后终止整个进程组并返回描述性错误。
func TestLocalTimeout(t *testing.T) {
	sb := &Local{Timeout: 200 * time.Millisecond}
	start := time.Now()
	out, err := sb.Run(context.Background(), "sleep 5")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("sleep should be killed by timeout, got out=%q", out)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v, should be killed near 200ms", elapsed)
	}
	if !strings.Contains(out, "[命令执行超时") {
		t.Fatalf("out should mark timeout, got %q", out)
	}
}

func TestLocalOutputLimit(t *testing.T) {
	out, err := (&Local{MaxOutput: 16}).Run(context.Background(), "seq 1 1000")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "[输出已截断") {
		t.Fatalf("out should be truncated, got %q", out)
	}
}

func TestLocalCPULimit(t *testing.T) {
	// macOS 不强制执行 RLIMIT_CPU（ulimit -t 无效，已知平台限制），仅在 Linux 上钉住。
	if runtime.GOOS == "darwin" {
		t.Skip("darwin 不强制 RLIMIT_CPU，ulimit -t 无效；本测试仅在 Linux 上验证")
	}
	sb := &Local{CpuSeconds: 1, Timeout: 10 * time.Second}
	start := time.Now()
	// yes 无限烧 CPU，唯一出路是 ulimit -t 1 的 SIGXCPU（10s 超时兜底防挂死）。
	out, err := sb.Run(context.Background(), "yes > /dev/null")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("cpu hog should be killed by ulimit, got out=%q (took %v)", out, elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %v, cpu limit should kill near 1s", elapsed)
	}
}

func TestLocalEnvironment(t *testing.T) {
	out, err := (&Local{}).Run(context.Background(), "env")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 白名单变量应当可见
	for _, want := range []string{"PATH=", "HOME="} {
		if !strings.Contains(out, want) {
			t.Fatalf("env should contain %q", want)
		}
	}
	// 父进程的自定义敏感变量不应泄漏
	if strings.Contains(out, "SECRET_KEY=") {
		t.Fatal("sensitive env vars should be stripped")
	}
}

// Default 兜底：进程组击杀存在（即使未注入 Runner 也能隔离）。
func TestDefaultProcessGroupKill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	out, err := Default{}.Run(ctx, "sleep 5")
	if err == nil {
		t.Fatalf("sleep should be killed, got out=%q", out)
	}
	if !strings.Contains(out, "[命令执行超时") {
		t.Fatalf("out should mark timeout, got %q", out)
	}
}

func TestDefaultEmptyCommand(t *testing.T) {
	if _, err := (Default{}).Run(context.Background(), ""); err == nil {
		t.Fatal("empty command should error")
	}
}

// FromContext 默认返回 Default{}（不是 nil），调用方可直接调用 Run。
func TestFromContextDefault(t *testing.T) {
	r := FromContext(context.Background())
	if r == nil {
		t.Fatal("FromContext must never return nil")
	}
	if _, ok := r.(Default); !ok {
		t.Fatalf("expected Default, got %T", r)
	}
}

func TestFromContextWithInjected(t *testing.T) {
	custom := &Local{WorkDir: "/tmp"}
	ctx := WithRunner(context.Background(), custom)
	r := FromContext(ctx)
	if r != custom {
		t.Fatalf("FromContext = %T, want *Local", r)
	}
}
