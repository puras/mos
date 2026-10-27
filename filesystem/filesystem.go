// Package filesystem 提供文件访问的 jail 能力：路径经 Lexical（词法前缀）+ Realpath
// （实体路径）双层校验后才可访问，越界返回 *PathDeniedError。
//
// 定位：S5 安全纵深的一层。它回答「这个路径能读/能写吗」，不回答「这次调用
// 允不允许调这个工具」（那是 agent/policy 的事），也不回答「进程跑在什么沙箱里」
// （那是 S5f sandbox 的事）。
//
// 框架不定义部署形态：Local / SaaS / 容器 是上层业务的事，框架只提供
// 「路径必须落在宿主声明的根内」这一机制。宿主按自己的形态声明 ReadRoots 与
// WriteRoots（见 FileAccessPolicy）。框架不内置“全开放”实现——要放开就把
// 根显式声明为 "/"，那是一条看得见的规则，而不是一个容易忘了的模式名。
//
// 三条纪律：
//   - fail-closed（不变式 1）：根为空就是拒绝该类操作，不放行；未装配文件系统
//     的工具返回 *NotConfiguredError。
//   - 读写都走双层校验：读同样走 Realpath——否则根内一个指向 /etc/passwd 的
//     symlink 就能把内容读出边界。
//   - 审计优先：越界拒绝不是静默失败，理由经 tool/result 回填模型并留痕。
//
// 诚实的边界（不夸大防护强度）：
//   - 本实现是「解析期校验 + 使用实体路径打开」，把校验与使用之间的窗口压到最小；
//     它**不是**真正的 TOCTOU 消除——彻底消除需要 openat2/O_NOFOLLOW 之类的
//     系统调用，Go 标准库给不了。它是纵深防御的一层，不是强隔离的替代品。
package filesystem

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Op 描述一次文件访问的类型（决定用哪套根集合校验）。
type Op string

const (
	OpRead  Op = "read"  // 读取内容
	OpWrite Op = "write" // 写入内容
)

// Request 描述一次文件访问请求。
type Request struct {
	// Path 是模型传入的路径：绝对路径，或相对于根锚点的相对路径。
	Path string
	// Op 决定用哪套根集合校验。
	Op Op
}

// Resolved 是解析后的结果。
type Resolved struct {
	// DisplayPath 是规范化后的路径（模型可见）。
	DisplayPath string
	// TargetKey 是用于实际打开的路径（经实体校验后的绝对路径）。
	TargetKey string
	// AllowedRoots 是本次请求适用的允许根集合。
	AllowedRoots []string
}

// FileSystem 是文件能力 seam：把「模型给的路径」解析成「可以安全访问的路径」。
type FileSystem interface {
	// Resolve 解析请求路径；越界返回 *PathDeniedError。
	Resolve(ctx context.Context, req Request) (*Resolved, error)
}

// PathDeniedError 是结构化的越界错误（拒绝理由会回填给模型，要写清楚原因）。
type PathDeniedError struct {
	Display      string // 模型传入的原始路径
	Target       string // 规范化后的绝对路径
	Op           Op
	AllowedRoots []string
}

func (e *PathDeniedError) Error() string {
	return fmt.Sprintf("路径 %q 不在允许的根内（%s 操作允许的根=%v）",
		e.Display, e.Op, e.AllowedRoots)
}

// NotConfiguredError 表示文件能力未装配（fail-closed：拒绝而不是放行）。
type NotConfiguredError struct{}

func (e *NotConfiguredError) Error() string {
	return "文件能力未装配（拒绝访问）"
}

// FileAccessPolicy 描述文件访问策略：**哪些根可以读、哪些根可以写**。
//
// 框架不预设形态：SaaS 平台通常是读写都在同一个工作区；本地开发可能读放开、
// 写受限；诊断/迁移场景可显式声明 "/" 完全开放。都是宿主自己的决定。
type FileAccessPolicy struct {
	// ReadRoots 是允许读取的根。空 = 读一律拒绝（fail-closed，不是“等于放开”）。
	ReadRoots []string
	// WriteRoots 是允许写入的根。空 = 写一律拒绝。
	//
	// 刻意**不含**隐式的 /tmp 与 os.TempDir()：共享临时目录是符号链接攻击的
	// 经典目标（别人放一个链接就让你写到别处），且多智能体并发时互相干扰。
	// 要用就显式写进来。
	WriteRoots []string
	// Anchor 是**相对路径的锚点**（相对路径挂在这个目录下）。空 = 默认取
	// 该类操作的第一个根（既有行为）。
	//
	// 运行期加根时锚点尤其重要：加根是**追加**，第一个根不变 → 锚点自然
	// 稳定，模型手里的相对路径不会悄悄指向别处。宿主想换锚点请显式写这里。
	Anchor string
}

// RootsFor 返回某类操作适用的根集合（去重、绝对化）。
func (p *FileAccessPolicy) RootsFor(op Op) []string {
	if p == nil {
		return nil
	}
	if op == OpWrite {
		return absAll(p.WriteRoots)
	}
	return absAll(p.ReadRoots)
}

// absAll 把根列表绝对化并去重（保持顺序）。
func absAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = absOrSelf(s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return dedupStr(out)
}

// absOrSelf 把路径绝对化；失败时原样返回（后续校验会拒绝它）。
func absOrSelf(s string) string {
	if s == "" {
		return s
	}
	a, err := filepath.Abs(s)
	if err != nil {
		return s
	}
	return a
}

// dedupStr 去重并保持顺序。
func dedupStr(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// underRoot 判断 target 是否在 root 子树内（词法判断，Lexical 阶段用）。
func underRoot(root, target string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
