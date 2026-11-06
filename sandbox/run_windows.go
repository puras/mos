//go:build windows

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	winsandbox "github.com/SivanCola/windows-sandbox"
)

// run 在 Windows 上执行命令。
//
// ## 路径强制：低完整性 token + 临时 ACL 授予
//
// Windows 没有 Landlock 那样"只收窄写"的内核原语，但有一个等价机制：
// **低完整性（Low integrity）token**。低完整性进程可以读普通文件，但
// **写不了**中完整性以上的对象（MIC 的"不可向上写"规则）；把写根标成
// 低完整性并临时授予写权限，就得到"只有写根能写、别处能读"的语义——
// 与 macOS/Linux 后端一致。
//
// 具体机制由 github.com/SivanCola/windows-sandbox 实施：
//
//   - 复制当前 token 并把完整性级别降到 Low；
//   - 给写根打上显式 Low 标签 + 临时写 ACE（**命令结束即回收**）；
//   - 子进程挂进 Job Object（关句柄即杀整树）；
//   - TEMP/TMP 重定向到本次命令的临时目录。
//
// ## 为什么中间加一层启动器
//
// 库自己 spawn 子进程，于是我们拿不到子进程的 PID——超时时没法精确杀树。
// 所以让它 spawn **我们自己**（重新执行），启动器再跑真正的命令：
//
//	父进程 --载荷管道(stdin)--> 启动器 --(继承 token)--> cmd.exe /C <命令>
//
// 启动器把 token 传给命令（子进程继承主 token），并在启动时把自己的 PID
// 写进 pid 文件——父进程超时读到 PID 后 taskkill /T 杀整棵树。
//
// 与 Linux 的 Landlock 启动器同一套形状（重新执行 + 管道载荷 + PID 回收）。
func (s *Local) run(ctx context.Context, req Request, eff *PathScope, timeout time.Duration) (Result, error) {
	dir, err := s.checkWorkDir()
	if err != nil {
		return Result{}, err
	}
	// 写全开（宿主显式声明写根 "/"）：不做路径强制，直接跑（危险写在脸上）。
	if writeOpen(eff) {
		argv := shellArgv(s.buildCommand(req.Command))
		return s.runExec(ctx, invocation{argv: argv}, dir, timeout)
	}
	return s.runWindowsSandboxed(ctx, req, eff, dir, timeout)
}

// runWindowsSandboxed 在低完整性 token 下跑命令（写范围由 ACL 强制）。
func (s *Local) runWindowsSandboxed(ctx context.Context, req Request, eff *PathScope, dir string, timeout time.Duration) (Result, error) {
	if !winsandbox.Available() {
		return Result{}, fmt.Errorf("%w: 本机没有可用的 Windows 沙箱原语（低完整性 token / Job Object 需要 Windows 10+）；当前只有进程级隔离（超时/整树终止/环境裁剪）；确需放开请在宿主显式声明写根 \"/\"，或把宿主跑在容器/WSL2 里（容器边界即路径边界）", ErrPathScopeUnsupported)
	}
	self, err := os.Executable()
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: 取宿主可执行文件路径失败（Windows 启动器要重新执行自己）: %w", err)
	}
	payload, err := json.Marshal(windowsPayload{Argv: shellArgv(s.buildCommand(req.Command))})
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: 编码 Windows 启动器载荷失败: %w", err)
	}
	if len(payload) > windowsPayloadMax {
		return Result{}, fmt.Errorf("sandbox: Windows 启动器载荷过大（%d 字节，上限 %d）", len(payload), windowsPayloadMax)
	}

	// 载荷管道：库只给三个标准句柄，载荷走 stdin 最省事（启动器读完就是 EOF，
	// 命令继承同一个 stdin → 看到空输入，等价于 /dev/null）。
	payloadR, payloadW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: 建 Windows 启动器载荷管道失败: %w", err)
	}
	defer payloadR.Close()
	if err := writeWindowsFrame(payloadW, payload); err != nil {
		_ = payloadW.Close()
		return Result{}, fmt.Errorf("sandbox: 写 Windows 启动器载荷失败: %w", err)
	}
	_ = payloadW.Close()

	// 输出管道：库要 *os.File（它自己 spawn 子进程），我们从读端收。
	outR, outW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: 建输出管道失败: %w", err)
	}
	defer outR.Close()
	defer outW.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: 建输出管道失败: %w", err)
	}
	defer errR.Close()
	defer errW.Close()

	// pid 文件：启动器把自己的 PID 写进来（父进程超时时要靠它杀整棵树）。
	pidFile, err := os.CreateTemp("", "mos-sandbox-*.pid")
	if err != nil {
		return Result{}, fmt.Errorf("sandbox: 建 pid 文件失败: %w", err)
	}
	pidPath := pidFile.Name()
	_ = pidFile.Close()
	_ = os.Remove(pidPath) // 留给启动器重建（WriteFile 会创建）

	env := append(s.environment(), windowsLaunchEnv+"=1", windowsPIDFileEnv+"="+pidPath)
	spec := winsandbox.Spec{
		WritableRoots: eff.WriteRoots,
		// SDK 不做网络强制（尽力而为：剥代理变量）→ 这里不限制网络。
		Network: true,
		// 可写模式：低完整性 token + 临时 ACL 授予（读仍不受限）。
		Writable:   true,
		TempPrefix: "mos-sandbox-",
	}
	opts := winsandbox.RunOptions{
		Stdin:  payloadR,
		Stdout: outW,
		Stderr: errW,
		Env:    env,
		Dir:    dir,
	}

	out := s.newOutputBuffer()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(out, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(out, errR) }()

	start := time.Now()
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := winsandbox.Run(spec, []string{self}, opts)
		done <- outcome{code: res.ExitCode, err: err}
	}()

	var res outcome
	timedOut := false
	select {
	case res = <-done:
	case <-ctx.Done():
		// 超时：启动器报了 PID → taskkill /T 杀整棵树（命令自己 fork 出的
		// 子进程也要死）。拿不到 PID 时只能靠库自己的 Job Object。
		if pid := readWindowsPID(pidPath); pid > 0 {
			killWindowsTree(pid)
		}
		res = <-done
		timedOut = true
	}
	wg.Wait()
	_ = os.Remove(pidPath)

	output := out.finish()
	result := Result{
		Output:     output,
		ExitCode:   res.code,
		Truncated:  out.truncated,
		OutputPath: out.finishSpill(),
		Duration:   time.Since(start),
	}
	if timedOut {
		return Result{
			Output:     output + "\n[命令执行超时，已终止]",
			ExitCode:   -1,
			Truncated:  out.truncated,
			OutputPath: out.finishSpill(),
			Duration:   time.Since(start),
		}, fmt.Errorf("sandbox: 命令执行超时（%v）", timeout)
	}
	// 启动器失败 = **沙箱没装上**（工具错误），不是命令的结果。
	if msg := launcherFailure(output); msg != "" {
		return result, errors.New(msg)
	}
	if res.err != nil {
		return result, fmt.Errorf("sandbox: Windows 沙箱执行失败: %w", res.err)
	}
	return result, nil
}

// readWindowsPID 读启动器报的 PID（没写好就返回 0）。
func readWindowsPID(path string) int {
	// 启动器在启动时就写了 pid 文件；给一点重试余量（文件可能被 antivirus
	// 之类的短暂占住）。
	for i := 0; i < 20; i++ {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(string(raw)); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return 0
}

// killWindowsTree 杀掉 pid 及其整棵进程树。
func killWindowsTree(pid int) {
	if pid <= 0 {
		return
	}
	if killer, err := exec.LookPath("taskkill"); err == nil {
		_ = exec.Command(killer, "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	}
}
