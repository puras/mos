package sandbox

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 命令在 WorkDir 里执行。
func TestRunsInWorkDir(t *testing.T) {
	dir := t.TempDir()
	res, err := (&Local{WorkDir: dir}).Run(context.Background(), Request{Command: "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, output=%q", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, dir) {
		t.Fatalf("输出应含工作目录 %q，实际 %q", dir, res.Output)
	}
}

// 超时：命令被杀，返回描述性错误。
func TestTimeoutKillsCommand(t *testing.T) {
	res, err := (&Local{Timeout: 200 * time.Millisecond}).Run(context.Background(),
		Request{Command: "sleep 30"})
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if res.ExitCode != -1 {
		t.Fatalf("被终止的 exit code 应为 -1，实际 %d", res.ExitCode)
	}
	if !strings.Contains(res.Output, "超时") {
		t.Fatalf("输出应说明超时: %q", res.Output)
	}
}

// 超时：子进程也一起死（进程组击杀）——否则 fork 出的孤儿会继续跑。
func TestTimeoutKillsChildProcesses(t *testing.T) {
	marker := t.TempDir() + "/child-ran"
	// 起一个子进程，它会往 marker 写文件；父进程等它。
	cmd := "sleep 1; touch " + marker + "; sleep 30"
	start := time.Now()
	_, err := (&Local{Timeout: 300 * time.Millisecond}).Run(context.Background(), Request{Command: cmd})
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("超时回收太慢: %v", d)
	}
	// 等一会儿确认子进程没来得及写
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("超时后子进程仍在运行（进程组没被整组杀掉）")
	}
}

// 输出上限：超过即截断并标记。
func TestOutputTruncated(t *testing.T) {
	res, err := (&Local{MaxOutput: 1024}).Run(context.Background(),
		Request{Command: "head -c 100000 /dev/zero | tr '\\0' 'x'"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("超限输出应标记截断")
	}
	if len(res.Output) > 1024+64 {
		t.Fatalf("输出长度应受上限约束, got %d", len(res.Output))
	}
	if !strings.Contains(res.Output, "输出已截断") {
		t.Fatalf("输出应有截断标记: %q", res.Output[len(res.Output)-40:])
	}
}

// 环境裁剪：白名单外的变量不传给命令。
func TestEnvironmentIsTrimmed(t *testing.T) {
	t.Setenv("MOS_SECRET_TOKEN", "super-secret")
	res, err := (&Local{}).Run(context.Background(), Request{Command: "echo [$MOS_SECRET_TOKEN]"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "super-secret") {
		t.Fatalf("白名单外的环境变量泄露了: %q", res.Output)
	}
}

// 非零退出码：不是 error，是"这次命令失败"的事实。
func TestNonZeroExitIsNotError(t *testing.T) {
	res, err := (&Local{}).Run(context.Background(), Request{Command: "echo oops >&2; exit 3"})
	if err != nil {
		t.Fatalf("非零退出码不该是 error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Output, "oops") {
		t.Fatalf("stderr 应被合并进输出: %q", res.Output)
	}
}

// 资源限制：CPU 时间上限预置生效（POSIX 平台都支持）。
func TestResourceLimitApplied(t *testing.T) {
	res, err := (&Local{CpuSeconds: 7}).Run(context.Background(),
		Request{Command: "ulimit -t"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "7") {
		t.Fatalf("CPU 时间上限应预置为 7 秒, 实际 %q", res.Output)
	}
}

// 平台不支持的限制被跳过，而不是让命令失败（macOS 的 ulimit -v 就是这样）。
func TestUnsupportedLimitDoesNotBreakCommand(t *testing.T) {
	res, err := (&Local{MemKB: 64 * 1024, CpuSeconds: 7}).Run(context.Background(),
		Request{Command: "echo ran"})
	if err != nil {
		t.Fatalf("命令不应因某条限制不被支持而失败: %v", err)
	}
	if !strings.Contains(res.Output, "ran") {
		t.Fatalf("命令应正常执行: %q", res.Output)
	}
}

// 空命令 → 报错，不执行。
func TestEmptyCommandRejected(t *testing.T) {
	if _, err := (&Local{}).Run(context.Background(), Request{Command: "  "}); err == nil {
		t.Fatal("空命令应报错")
	}
}

// 工作目录不可用 → 报错，不执行。
func TestBadWorkDirRejected(t *testing.T) {
	if _, err := (&Local{WorkDir: "/definitely/not/here"}).Run(context.Background(),
		Request{Command: "pwd"}); err == nil {
		t.Fatal("工作目录不可用应报错")
	}
}

// NoNet：代理变量被剥离。
func TestNoNetStripsProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:8080")
	res, err := (&Local{NoNet: true}).Run(context.Background(), Request{Command: "echo [$HTTPS_PROXY]"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "proxy.invalid") {
		t.Fatalf("代理变量应被剥离: %q", res.Output)
	}
}

// 编译期断言：Runner 接口的实现集合。
var (
	_ Runner = (*Local)(nil)
	_        = exec.Command
)
