// Package sandbox 提供 shell 命令的隔离执行能力（capability seam）。
//
// Runner 是公共接口；具体实现：
//   - Local：完整六层隔离（工作目录 jail + 进程组击杀 + ulimit + 环境裁剪 + 输出截断 + 网络隔离尽力而为）
//   - Default：兜底实现，含进程组击杀与超时截断，无 ulimit/环境裁剪（修复 v2.23 前 Shell 默认路径无沙箱的 bug）
//
// 诚实的边界：本包是本地轻量实现；完整网络命名空间隔离依赖远程沙箱后端（M4：Docker / containerd）。
// 原位于 tool/sandbox.go；v2.24 拆为顶层包以统一 capability 包结构（与 filesystem / httpx 平级）。
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Runner 是命令执行隔离器的抽象（capability seam）。
// 平台可注入自定义 Runner（如 Docker / containerd 后端）。
type Runner interface {
	// Run 在隔离环境中执行命令，返回（stdout+stderr 合并输出, 执行错误）。
	// 执行错误为 *exec.ExitError 时表示命令正常结束但退出码非零；超时返回描述性错误。
	Run(ctx context.Context, command string) (string, error)
}

// Local 是完整六层隔离的命令执行器（M1.6 执行沙箱，本地轻量实现）。
// 它回答纵深防御的隔离层问题：Policy 拦截"该不该调用"，沙箱限制"放行后能造成多大破坏"。
//
// 隔离手段：
//   - 工作目录 jail：命令在 WorkDir 内执行；
//   - 单次调用超时：超时后终止整个进程组（含子进程，避免孤儿）；
//   - 输出大小上限：超出即截断并丢弃后续内容；
//   - 进程资源限制：CPU 时间 / 虚拟内存 / 单文件大小（经 ulimit 预置，0 表示不限制）；
//   - 环境裁剪：仅保留白名单环境变量，NoNet 时剥离代理配置；
//   - 网络隔离尽力而为：裁剪代理环境变量（无法阻止直连）。
type Local struct {
	WorkDir    string        // 命令执行根目录（jail）；空使用进程当前目录
	Timeout    time.Duration // 单次调用超时；0 使用默认 10s
	MaxOutput  int64         // 输出上限（字节）；0 使用默认 64KB
	CpuSeconds int64         // CPU 时间上限（秒）；0 不限制
	MemKB      int64         // 虚拟内存上限（KB）；0 不限制
	MaxFileKB  int64         // 单文件大小上限（KB）；0 不限制
	NoNet      bool          // 网络隔离（尽力而为：剥离代理环境变量）
}

const (
	sandboxDefaultTimeout = 10 * time.Second
	sandboxDefaultMaxOut  = 64 << 10 // 64KB
)

// Run 实现 Runner：完整六层隔离的命令执行。
func (s *Local) Run(ctx context.Context, command string) (string, error) {
	if s == nil {
		return "", errors.New("sandbox: nil sandbox")
	}
	if strings.TrimSpace(command) == "" {
		return "", errors.New("sandbox: empty command")
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = sandboxDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := s.WorkDir
	if dir != "" {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("sandbox: 工作目录 %q 不可用: %w", dir, err)
		}
	}

	cmd := exec.Command("/bin/sh", "-c", s.buildCommand(command))
	cmd.Dir = dir
	cmd.Env = s.environment()
	// 组隔离：子进程单独进程组，超时终止时可整组击杀，避免孤儿进程继续运行
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	maxOut := s.MaxOutput
	if maxOut <= 0 {
		maxOut = sandboxDefaultMaxOut
	}
	var out limitedBuffer
	out.max = maxOut
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("sandbox: 启动命令失败: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return out.finish(), err
	case <-ctx.Done():
		// 终止整个进程组；若命令已自行结束则忽略错误
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return out.finish() + "\n[命令执行超时，已终止]", fmt.Errorf("sandbox: 命令执行超时（%v）", timeout)
	}
}

// buildCommand 用 ulimit 预置进程资源限制（软限制，仅作用于本命令及其子进程）。
func (s *Local) buildCommand(command string) string {
	var limits []string
	if s.CpuSeconds > 0 {
		limits = append(limits, fmt.Sprintf("ulimit -t %d", s.CpuSeconds))
	}
	if s.MemKB > 0 {
		limits = append(limits, fmt.Sprintf("ulimit -v %d", s.MemKB))
	}
	if s.MaxFileKB > 0 {
		limits = append(limits, fmt.Sprintf("ulimit -f %d", s.MaxFileKB))
	}
	if len(limits) == 0 {
		return command
	}
	return strings.Join(limits, "; ") + "; " + command
}

// environment 裁剪环境变量：仅保留白名单变量，避免向模型执行的命令泄露主机敏感配置。
func (s *Local) environment() []string {
	allowed := []string{"PATH", "HOME", "LANG", "TMPDIR", "PWD", "USER", "SHELL", "LC_ALL"}
	env := make([]string, 0, len(allowed)+2)
	for _, key := range allowed {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	if s.NoNet {
		env = append(env,
			"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "http_proxy=", "https_proxy=", "all_proxy=",
			"NO_PROXY=*", "no_proxy=*",
		)
	}
	return env
}

// Default 是 Runner 的兜底实现：含进程组击杀 + 超时 + 输出截断，但无 ulimit / 无环境裁剪。
// 用于 ctx 未注入 Runner 时的回退，确保即使平台忘记配 Runner 也至少有进程组隔离
// （修复 v2.23 前 Shell 默认路径无沙箱的 bug——只调 exec.CommandContext 不杀子进程）。
type Default struct{}

// Run 实现 Runner：兜底隔离。
// 默认 10s 超时 + 64KB 输出截断 + 进程组击杀（Setpgid + SIGKILL 进程组）。
// 不做 ulimit / 不裁剪环境变量——避免对默认调用方产生意外的副作用。
func (Default) Run(ctx context.Context, command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("sandbox: empty command")
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxDefaultTimeout)
	defer cancel()

	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var out limitedBuffer
	out.max = sandboxDefaultMaxOut
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("sandbox: 启动命令失败: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return out.finish(), err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return out.finish() + "\n[命令执行超时，已终止]", fmt.Errorf("sandbox: 命令执行超时（%v）", sandboxDefaultTimeout)
	}
}

// limitedBuffer 是带输出上限的缓冲 writer：达到上限后丢弃后续内容并标记截断。
// 丢弃而非阻塞，避免子进程因管道满而挂死（资源限制与超时兜底）。
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.truncated {
		return len(p), nil
	}
	remaining := b.max - int64(b.buf.Len())
	if int64(len(p)) <= remaining {
		return b.buf.Write(p)
	}
	b.buf.Write(p[:remaining])
	b.truncated = true
	return len(p), nil
}

// finish 返回最终输出，截断时追加标记。
func (b *limitedBuffer) finish() string {
	out := b.buf.String()
	if b.truncated {
		out += "\n[输出已截断，超过上限]"
	}
	return out
}

// ---- ctx 注入：工具从上下文取 Runner ----

type ctxKey struct{}

// WithRunner 把 Runner 注入 ctx。
func WithRunner(ctx context.Context, r Runner) context.Context {
	return context.WithValue(ctx, ctxKey{}, r)
}

// FromContext 从 ctx 取出 Runner；未注入返回 Default{}（含进程组击杀的兜底）。
// 永远不返回 nil——Shell 工具不再有"默认路径无沙箱"的 bug。
func FromContext(ctx context.Context) Runner {
	r, _ := ctx.Value(ctxKey{}).(Runner)
	if r != nil {
		return r
	}
	return Default{}
}

// Compile-time 接口断言
var (
	_ Runner = (*Local)(nil)
	_ Runner = Default{}
)
