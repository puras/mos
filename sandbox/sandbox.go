// Package sandbox 提供 shell 命令的隔离执行能力（capability seam）。
//
// Runner 是公共接口；本地实现 Local 提供**进程级**隔离。平台可注入更强的后端
// （Docker / containerd / bwrap / Landlock），接口不变（PRD 的 FR-E2）。
//
// ## 隔离了什么（六层，照归档实现）
//
//	① 工作目录：命令在宿主指定的目录里跑（相对路径的默认落点）
//	② 超时 + 进程组击杀：Setpgid 后整组 SIGKILL——fork 出的孤儿进程一起死
//	③ 输出上限：超过上限就丢弃后续内容（丢弃而非阻塞，避免管道写满挂死）
//	④ 资源限制：ulimit 预置 CPU 时间 / 虚拟内存 / 单文件大小
//	⑤ 环境裁剪：只保留白名单变量，别把主机的密钥泄给模型执行的命令
//	⑥ 网络：剥离代理环境变量（尽力而为）
//
// ## 诚实的边界（不夸大防护强度）
//
//   - **限资源，不限文件**：本实现限制的是"这次运行能消耗多少"（时间 / CPU /
//     内存 / 输出 / 子进程），**不限制命令能碰哪些文件**——命令仍能访问运行用户
//     权限内的任意路径。路径范围的强制隔离需要外部后端（bwrap / Landlock /
//     Seatbelt / 容器），Go 标准库给不了。命令级的语义约束属策略层（S5f-2）。
//   - **网络隔离是尽力而为**：剥离代理变量挡不住直连。真隔离要靠网络命名空间。
//   - 因此本包是**纵深防御的一层**，不是强隔离的替代品。
//
// ## fail-closed
//
// 未装配 Runner 的调用方拿不到可执行的能力：工具在 nil Runner 时直接拒绝，
// 不存在"没配沙箱就跑裸命令"的默认路径（归档曾有该缺陷，用兜底 Runner 修）。
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

const (
	// DefaultTimeout 是单次命令的默认超时（宿主未配置时的缺省）。
	//
	// 30 秒：一次失控的命令不该把会话挂住太久。DSH 的缺省是 120s（为慢的
	// `go test` 留的），宿主配 Local.Timeout 即可放宽——缺省保持保守。
	DefaultTimeout = 30 * time.Second
	// DefaultMaxOutput 是单次命令的输出上限（字节）。
	DefaultMaxOutput = 64 << 10 // 64KB
)

// defaultEnvAllow 是环境变量白名单（PATH/HOME/LANG/…）——别把主机的密钥泄给
// 模型执行的命令。
var defaultEnvAllow = []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "PWD", "USER", "SHELL", "TERM"}

// Request 是一次命令执行请求。
type Request struct {
	// Command 是要执行的 shell 命令（交给 /bin/sh -c 解释）。
	Command string
	// Timeout 是本次调用的超时；<=0 时由实现取默认值。
	Timeout time.Duration
	// Note：工作目录**不由调用方指定**——那会给模型一个逃逸口（cd 到任意
	// 目录）。工作目录是宿主的配置（Local.WorkDir）。
}

// Result 是一次命令执行的结果。
type Result struct {
	// Output 是 stdout 与 stderr 的合并输出（可能被截断）。
	Output string
	// ExitCode 是退出码；被超时终止时为 -1。
	ExitCode int
	// Truncated 标记输出是否因超限被截断。
	Truncated bool
	// OutputPath 是完整输出的落盘路径（仅当截断且配置了 SpillDir 时非空）。
	// 内存里只留前 MaxOutput 字节，全文在这里——模型可以 read_file 读它
	// （PI 的 full_output_path 对位，PRD 的"落盘逃生通道"）。
	OutputPath string
	// Duration 是实际耗时。
	Duration time.Duration
}

// Runner 是命令执行器（capability seam）。
type Runner interface {
	// Run 执行命令并返回结果。命令自身失败（退出码非零）不是 error——那是
	// "这次调用失败"的事实，由调用方回填给模型；只有**无法执行**（启动失败、
	// 超时）才返回 error。
	Run(ctx context.Context, req Request) (Result, error)
}

// NotConfiguredError 表示沙箱能力未装配（fail-closed：拒绝执行，不跑裸命令）。
type NotConfiguredError struct{}

func (e *NotConfiguredError) Error() string {
	return "命令执行能力未装配（拒绝执行）"
}

// Local 是本地进程级隔离的执行器。
//
// 隔离手段见包注释的六层。零值可用：WorkDir 空表示用进程当前目录，Timeout
// 空用 DefaultTimeout，其余限制为 0 表示不限制。
type Local struct {
	// WorkDir 是命令执行目录（宿主配置，不交给模型指定）。
	WorkDir string
	// Timeout 是单次调用的超时，**既是缺省也是上限**。
	//
	// 模型可以在调用里要求更短的超时，但不能要求更长——上限由宿主定（模型请求
	// 超过它会被夹到它，避免"模型要求跑一小时"绕过宿主限制）。
	// <=0 时用 DefaultTimeout。
	Timeout time.Duration
	// MaxOutput 是输出上限（字节）；<=0 用 DefaultMaxOutput。
	MaxOutput int64
	// CpuSeconds 是 CPU 时间上限（秒）；0 = 不限制。
	CpuSeconds int64
	// MemKB 是虚拟内存上限（KB）；0 = 不限制。
	MemKB int64
	// MaxFileKB 是单文件大小上限（KB）；0 = 不限制。
	MaxFileKB int64
	// NoNet 剥离代理环境变量（尽力而为的网络隔离）。
	NoNet bool
	// EnvAllow 是环境变量白名单；空用内置白名单。
	EnvAllow []string
	// SpillDir 是输出落盘目录（可选，空 = 只截断不落盘）。
	// 输出超限时完整输出写入这里（内存只留前 MaxOutput 字节）。
	// 宿主显式指定：默认的 os.TempDir() 在文件 jail 的可写根之外
	// （共享临时目录是符号链接攻击的经典目标）。
	SpillDir string
}

var _ Runner = (*Local)(nil)

// Run 实现 Runner。
func (s *Local) Run(ctx context.Context, req Request) (Result, error) {
	if s == nil {
		return Result{}, &NotConfiguredError{}
	}
	if strings.TrimSpace(req.Command) == "" {
		return Result{}, errors.New("sandbox: 命令不能为空")
	}

	timeout := req.Timeout
	if s.Timeout > 0 {
		// 宿主的 Timeout 既是缺省也是上限：模型不能要求更久。
		if timeout <= 0 || timeout > s.Timeout {
			timeout = s.Timeout
		}
	} else if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := s.WorkDir
	if dir != "" {
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			return Result{}, fmt.Errorf("sandbox: 工作目录 %q 不可用: %w", dir, err)
		}
	}

	cmd := exec.Command("/bin/sh", "-c", s.buildCommand(req.Command))
	cmd.Dir = dir
	cmd.Env = s.environment()
	// 进程组：子进程单独成组，超时终止时整组击杀，避免孤儿继续跑
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	maxOut := s.MaxOutput
	if maxOut <= 0 {
		maxOut = DefaultMaxOutput
	}
	var out limitedBuffer
	out.max = maxOut
	out.spillDir = s.SpillDir
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("sandbox: 启动命令失败: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		res := Result{
			Output:     out.finish(),
			ExitCode:   exitCodeOf(err),
			Truncated:  out.truncated,
			OutputPath: out.finishSpill(),
			Duration:   time.Since(start),
		}
		return res, nil
	case <-ctx.Done():
		// 整组击杀：命令自己 fork 出的子进程也要死
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return Result{
			Output:     out.finish() + "\n[命令执行超时，已终止]",
			ExitCode:   -1,
			Truncated:  out.truncated,
			OutputPath: out.finishSpill(),
			Duration:   time.Since(start),
		}, fmt.Errorf("sandbox: 命令执行超时（%v）", timeout)
	}
}

// buildCommand 用 ulimit 预置进程资源限制（软限制，作用于本命令及其子进程）。
//
// 每条限制都带 `|| true`：**平台支持性不同**——macOS 的 `ulimit -v`（虚拟内存）
// 直接报 "cannot modify limit"，若不吞掉这个错误，整条命令都跑不起来。
// 换句话说：平台不支持的限制被**跳过**，而不是让命令失败。
//
// 支持情况：CPU 时间（-t）与单文件大小（-f）POSIX 平台都支持；虚拟内存（-v）
// 只有 Linux 等平台支持，macOS 上不生效（诚实边界，见包注释）。
func (s *Local) buildCommand(command string) string {
	var limits []string
	if s.CpuSeconds > 0 {
		limits = append(limits, fmt.Sprintf("ulimit -t %d 2>/dev/null || true", s.CpuSeconds))
	}
	if s.MemKB > 0 {
		limits = append(limits, fmt.Sprintf("ulimit -v %d 2>/dev/null || true", s.MemKB))
	}
	if s.MaxFileKB > 0 {
		// ulimit -f 的单位是 512 字节块，不是 KB——换算一下（归档写错了单位）。
		limits = append(limits, fmt.Sprintf("ulimit -f %d 2>/dev/null || true", s.MaxFileKB*2))
	}
	if len(limits) == 0 {
		return command
	}
	return strings.Join(limits, "; ") + "; " + command
}

// environment 裁剪环境变量：只保留白名单项，避免把主机敏感配置泄给模型执行的命令。
func (s *Local) environment() []string {
	allow := s.EnvAllow
	if len(allow) == 0 {
		allow = defaultEnvAllow
	}
	env := make([]string, 0, len(allow)+8)
	for _, key := range allow {
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

// exitCodeOf 从 exec 错误里取退出码；正常结束为 0。
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// limitedBuffer 是带输出上限的缓冲 writer：内存里只留前 max 字节，超限后丢弃
// 后续内容并标记截断。
//
// 丢弃而非阻塞，避免子进程因管道写满而挂死。若配置了 spillDir，完整输出会
// 另存一份到文件——截断丢掉的内容因此**还能找回**（模型可 read_file 读它）。
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int64
	truncated bool
	spillDir  string
	spill     *os.File
	spillPath string
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.truncated {
		b.writeSpill(p)
		return len(p), nil
	}
	remaining := b.max - int64(b.buf.Len())
	if int64(len(p)) <= remaining {
		return b.buf.Write(p)
	}
	keep := int(remaining)
	if keep < 0 {
		keep = 0
	}
	if keep > len(p) {
		keep = len(p)
	}
	b.buf.Write(p[:keep])
	b.truncated = true
	b.openSpill()
	b.writeSpill(p[keep:])
	return len(p), nil
}

// openSpill 建落盘文件，并把已缓冲的内容写进去（保证文件是完整输出）。
func (b *limitedBuffer) openSpill() {
	if b.spillDir == "" || b.spill != nil {
		return
	}
	if err := os.MkdirAll(b.spillDir, 0o755); err != nil {
		return
	}
	f, err := os.CreateTemp(b.spillDir, "mos-bash-*.log")
	if err != nil {
		return
	}
	b.spill = f
	b.spillPath = f.Name()
	_, _ = b.spill.Write(b.buf.Bytes())
}

// writeSpill 把内容追加到落盘文件（落盘失败不影响命令执行）。
func (b *limitedBuffer) writeSpill(p []byte) {
	if b.spill == nil || len(p) == 0 {
		return
	}
	_, _ = b.spill.Write(p)
}

// finishSpill 关闭落盘文件并返回其路径。
func (b *limitedBuffer) finishSpill() string {
	if b.spill != nil {
		_ = b.spill.Close()
		b.spill = nil
	}
	return b.spillPath
}

// finish 返回最终输出，截断时追加标记。
func (b *limitedBuffer) finish() string {
	out := b.buf.String()
	if b.truncated {
		out += "\n[输出已截断，超过上限]"
	}
	return out
}
