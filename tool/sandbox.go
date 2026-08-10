package tool

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

// Sandbox 是 shell 命令的隔离执行器（M1.6 执行沙箱，本地轻量实现）。
// 它回答纵深防御的隔离层问题：Policy 拦截"该不该调用"，沙箱限制"放行后能造成多大破坏"。
//
// 隔离手段：
//   - 工作目录 jail：命令在 WorkDir 内执行；
//   - 单次调用超时：超时后终止整个进程组（含子进程，避免孤儿）；
//   - 输出大小上限：超出即截断并丢弃后续内容；
//   - 进程资源限制：CPU 时间 / 虚拟内存 / 单文件大小（经 ulimit 预置，0 表示不限制）；
//   - 环境裁剪：仅保留白名单环境变量，NoNet 时剥离代理配置。
//
// 诚实的边界：本地实现的网络隔离是尽力而为（裁剪代理变量无法阻止直连）；
// 完整网络命名空间隔离依赖远程沙箱后端（M4：Docker / containerd），不在本实现范围内。
type Sandbox struct {
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

// Run 在沙箱内执行 shell 命令，返回（stdout+stderr 合并输出, 执行错误）。
// 执行错误为 *exec.ExitError 时表示命令正常结束但退出码非零；超时返回描述性错误。
func (s *Sandbox) Run(ctx context.Context, command string) (string, error) {
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
func (s *Sandbox) buildCommand(command string) string {
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
func (s *Sandbox) environment() []string {
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

// ---- ctx 注入：工具从上下文取沙箱，未配置时用默认执行方式 ----

type sandboxCtxKey struct{}

// WithSandbox 将沙箱放入上下文，供工具执行时取用。
func WithSandbox(ctx context.Context, s *Sandbox) context.Context {
	return context.WithValue(ctx, sandboxCtxKey{}, s)
}

// SandboxFrom 从上下文取沙箱；未配置时返回 nil。
func SandboxFrom(ctx context.Context) *Sandbox {
	s, _ := ctx.Value(sandboxCtxKey{}).(*Sandbox)
	return s
}
