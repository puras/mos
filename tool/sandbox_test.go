package tool

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxRunBasic(t *testing.T) {
	out, err := (&Sandbox{}).Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Fatalf("out = %q, want hello", out)
	}
}

func TestSandboxEmptyCommand(t *testing.T) {
	if _, err := (&Sandbox{}).Run(context.Background(), "   "); err == nil {
		t.Fatal("empty command should error")
	}
}

// 工作目录 jail：命令在 WorkDir 内执行。
func TestSandboxWorkDirJail(t *testing.T) {
	dir := t.TempDir()
	out, err := (&Sandbox{WorkDir: dir}).Run(context.Background(), "pwd")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if strings.TrimSpace(out) != want {
		t.Fatalf("pwd = %q, want %q", strings.TrimSpace(out), want)
	}
}

func TestSandboxInvalidWorkDir(t *testing.T) {
	sb := &Sandbox{WorkDir: filepath.Join(t.TempDir(), "not-exist")}
	if _, err := sb.Run(context.Background(), "pwd"); err == nil {
		t.Fatal("invalid workdir should error")
	}
}

// 单次调用超时：超时后终止整个进程组并返回描述性错误。
func TestSandboxTimeout(t *testing.T) {
	sb := &Sandbox{Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err := sb.Run(context.Background(), "sleep 5")
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout not enforced promptly: %v", time.Since(start))
	}
}

// 输出上限：超出即截断并标记。
func TestSandboxOutputLimit(t *testing.T) {
	out, err := (&Sandbox{MaxOutput: 16}).Run(context.Background(), "seq 1 1000")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "[输出已截断") {
		t.Fatalf("out = %q, want truncation marker", out)
	}
}

// CPU 时间上限：死循环被 ulimit -t 终止（或超时兜底），不会无限运行。
func TestSandboxCPULimit(t *testing.T) {
	sb := &Sandbox{CpuSeconds: 1, Timeout: 10 * time.Second}
	start := time.Now()
	_, err := sb.Run(context.Background(), "while true; do :; done")
	if err == nil {
		t.Fatal("infinite loop should be killed by CPU limit")
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("cpu limit not enforced: %v", time.Since(start))
	}
}

// 环境裁剪：沙箱内仅保留白名单变量。
// 注：SHLVL 是 /bin/sh 启动时自产自销的 shell 层级变量（非主机环境透传），予以豁免。
func TestSandboxEnvironment(t *testing.T) {
	out, err := (&Sandbox{}).Run(context.Background(), "env")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "LANG": true, "TMPDIR": true,
		"PWD": true, "USER": true, "SHELL": true, "LC_ALL": true,
		"SHLVL": true, // shell 自产变量
		"_":     true, // shell 记录上一条命令路径的变量
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		key := strings.SplitN(line, "=", 2)[0]
		if !allowed[key] {
			t.Fatalf("unexpected env var in sandbox: %q", line)
		}
	}
}

// NoNet：尽力而为的网络隔离——剥离代理配置（不暴露主机代理地址）。
func TestSandboxNoNet(t *testing.T) {
	out, err := (&Sandbox{NoNet: true}).Run(context.Background(), "env | grep -i proxy; true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		// 允许 NO_PROXY=*，其余代理变量必须为空值或不存在
		if strings.HasPrefix(line, "NO_PROXY") || strings.HasPrefix(line, "no_proxy") {
			continue
		}
		if !strings.HasSuffix(line, "=") {
			t.Fatalf("proxy env not stripped: %q", line)
		}
	}
}

// ctx 注入：WithSandbox / SandboxFrom 往返。
func TestSandboxContext(t *testing.T) {
	ctx := context.Background()
	if SandboxFrom(ctx) != nil {
		t.Fatal("bare ctx should have no sandbox")
	}
	sb := &Sandbox{WorkDir: "/tmp"}
	ctx = WithSandbox(ctx, sb)
	if got := SandboxFrom(ctx); got != sb {
		t.Fatalf("SandboxFrom = %v, want %v", got, sb)
	}
}
