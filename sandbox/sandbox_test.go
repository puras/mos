package sandbox

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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

// 缺省取宿主的 Timeout：模型不指定就用它。
func TestHostTimeoutIsDefault(t *testing.T) {
	sb := &Local{Timeout: 300 * time.Millisecond, MaxTimeout: 10 * time.Second}
	start := time.Now()
	_, err := sb.Run(context.Background(), Request{Command: "sleep 30"})
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("应按宿主缺省(300ms)终止, 实际 %v", d)
	}
}

// 上界独立于缺省：宿主给 300ms 缺省 + 2s 上界，模型要 10s → 按上界 2s 终止。
// 这正是"两个旋钮"的意义：缺省保守，但允许模型在慢任务上申请更多（到上界）。
func TestMaxTimeoutAllowsLongerThanDefault(t *testing.T) {
	sb := &Local{Timeout: 300 * time.Millisecond, MaxTimeout: 2 * time.Second}
	start := time.Now()
	_, err := sb.Run(context.Background(), Request{Command: "sleep 30", Timeout: 10 * time.Second})
	if err == nil {
		t.Fatal("应超时")
	}
	d := time.Since(start)
	if d < 1500*time.Millisecond {
		t.Fatalf("应按上界(2s)而非缺省(300ms)终止, 实际 %v", d)
	}
	if d > 4*time.Second {
		t.Fatalf("应按上界(2s)终止, 实际 %v", d)
	}
}

// 没配上界 → 退化为缺省（不放宽）：模型不能要求更久。
func TestMaxTimeoutDefaultsToTimeout(t *testing.T) {
	sb := &Local{Timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := sb.Run(context.Background(), Request{Command: "sleep 30", Timeout: 10 * time.Second})
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("没配上界时应按缺省终止, 实际 %v", d)
	}
}

// 只配上界、没配缺省：缺省仍是框架缺省(30s)，上界单独生效。
func TestMaxTimeoutWithoutDefault(t *testing.T) {
	sb := &Local{MaxTimeout: 400 * time.Millisecond}
	start := time.Now()
	_, err := sb.Run(context.Background(), Request{Command: "sleep 30", Timeout: 10 * time.Second})
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("应按上界(400ms)终止, 实际 %v", d)
	}
}

// 模型要求更短 → 尊重模型的（收紧自己的额度永远允许）。
func TestShorterModelTimeoutIsHonored(t *testing.T) {
	sb := &Local{Timeout: 10 * time.Second, MaxTimeout: 30 * time.Second}
	start := time.Now()
	_, err := sb.Run(context.Background(), Request{Command: "sleep 30", Timeout: 200 * time.Millisecond})
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("应按模型给的 200ms 终止, 实际 %v", d)
	}
}

// 动态预算：宿主在运行期换预算，缺省与上界都跟着变。
func TestDynamicBudgetOverridesStatic(t *testing.T) {
	var cell atomic.Value
	cell.Store(Budget{Default: 200 * time.Millisecond, Max: 400 * time.Millisecond})
	sb := &Local{Timeout: 10 * time.Second, MaxTimeout: 20 * time.Second}
	sb.Budget = func() Budget { return cell.Load().(Budget) }

	// 静态值是 10s/20s，动态值 200ms/400ms —— 动态优先
	start := time.Now()
	_, err := sb.Run(context.Background(), Request{Command: "sleep 30", Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("应超时")
	}
	d := time.Since(start)
	if d > 2*time.Second {
		t.Fatalf("应按动态上界(400ms)终止, 实际 %v", d)
	}

	// 运行期把预算放宽 → 立刻生效（要 1.5s，动态上界 2s 允许；若还按旧的
	// 400ms 夹住，这个断言会失败）
	cell.Store(Budget{Default: time.Second, Max: 2 * time.Second})
	start = time.Now()
	_, err = sb.Run(context.Background(), Request{Command: "sleep 30", Timeout: 1500 * time.Millisecond})
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d < time.Second {
		t.Fatalf("放宽后应按 1.5s 终止, 实际 %v", d)
	}
}

// 动态预算返回零值 → 回落到静态字段（不丢配置）。
func TestDynamicBudgetZeroFallsBackToStatic(t *testing.T) {
	sb := &Local{Timeout: 300 * time.Millisecond, MaxTimeout: 300 * time.Millisecond}
	sb.Budget = func() Budget { return Budget{} }
	start := time.Now()
	_, err := sb.Run(context.Background(), Request{Command: "sleep 30", Timeout: 10 * time.Second})
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("应回落到静态上界(300ms)终止, 实际 %v", d)
	}
}

// 动态预算只给 Max 不给 Default → 缺省回落到静态，上界用动态的。
func TestDynamicBudgetMaxOnly(t *testing.T) {
	sb := &Local{Timeout: 200 * time.Millisecond}
	sb.Budget = func() Budget { return Budget{Max: 5 * time.Second} }
	// 模型要 3s（静态上界本会夹到 200ms，动态上界 5s 允许）
	start := time.Now()
	_, err := sb.Run(context.Background(), Request{Command: "sleep 30", Timeout: 3 * time.Second})
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("应按模型的 3s 终止, 实际 %v", d)
	}
}

// 动态预算的 Max 未给 → 不比缺省更宽（没给上界就不给模型加钱的余地）。
func TestDynamicBudgetWithoutMaxIsNotWider(t *testing.T) {
	sb := &Local{}
	sb.Budget = func() Budget { return Budget{Default: 300 * time.Millisecond} }
	start := time.Now()
	_, err := sb.Run(context.Background(), Request{Command: "sleep 30", Timeout: 10 * time.Second})
	if err == nil {
		t.Fatal("应超时")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("应按动态缺省(300ms)终止, 实际 %v", d)
	}
}

// 宿主在业务里热更新预算，同时命令在跑 —— 不能有数据竞争。
func TestDynamicBudgetConcurrentUpdateIsRaceFree(t *testing.T) {
	var cell atomic.Value
	cell.Store(Budget{Default: time.Second, Max: 2 * time.Second})
	sb := &Local{}
	sb.Budget = func() Budget { return cell.Load().(Budget) }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cell.Store(Budget{Default: time.Duration(i+1) * time.Millisecond, Max: 2 * time.Second})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			sb.Run(context.Background(), Request{Command: "echo hi"}) //nolint:errcheck
		}
	}()
	wg.Wait()
}
