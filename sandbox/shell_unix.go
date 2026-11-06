//go:build unix

package sandbox

import (
	"fmt"
	"strings"
)

// shellArgv 把一条命令交给平台外壳解释。
//
// unix：/bin/sh -c（POSIX 语法，Linux/macOS 一致）。
func shellArgv(command string) []string {
	return []string{"/bin/sh", "-c", command}
}

// resourcePrefix 预置进程资源限制（软限制，作用于本命令及其子进程）。
//
// 每条限制都带 `|| true`：**平台支持性不同**——macOS 的 `ulimit -v`（虚拟内存）
// 直接报 "cannot modify limit"，若不吞掉这个错误，整条命令都跑不起来。
// 换句话说：平台不支持的限制被**跳过**，而不是让命令失败。
//
// 支持情况：CPU 时间（-t）与单文件大小（-f）POSIX 平台都支持；虚拟内存（-v）
// 只有 Linux 等平台支持，macOS 上不生效（诚实边界，见包注释）。
func (s *Local) resourcePrefix() string {
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
	return strings.Join(limits, "; ")
}
