// Package filesystem 实现文件系统 capability seam（架构 v2 §3.5，对齐 dsh ctx.fs + sandboxPolicy）。
// Resolve 在 Lexical + Realpath 双层校验后返回可访问的目标路径；越界返回 *PathDeniedError。
// 原位于 tool/fs；v2.24 拆为顶层包以统一 capability 包结构（与 sandbox / httpx 平级）。
package filesystem

import (
	"context"
	"fmt"
)

// Mode 描述 fs 访问模式（DSH sandboxMode 镜像）。
type Mode string

const (
	ModeLocalJailed Mode = "local-jailed" // DSH workspace-write：写 jail，读开放
	ModeSaaSJailed  Mode = "saas-jailed"  // DSH read-only / SaaS 默认：读写都 jail
	ModeDangerFull  Mode = "danger-full"  // 显式全开放（用户主动设置；诊断/迁移专用）
)

// Op 描述一次 fs 访问的类型（用于按读写严格度分流）。
type Op string

const (
	OpRead  Op = "read"  // ReadFile
	OpWrite Op = "write" // WriteFile
	OpList  Op = "list"  // FindFiles / ls
	OpGrep  Op = "grep"  // Grep
)

// Request 描述一次 fs 访问请求。
type Request struct {
	Path      string // 模型传入的路径（可能含 .. 或绝对路径）
	Op        Op     // 决定严格度
	SessionID string // 审计/日志关联
}

// Resolved 是解析后的结果。
type Resolved struct {
	DisplayPath  string   // 模型可见的路径
	TargetKey    string   // realpath 后的实体路径（用于 os.Open）
	AllowedRoots []string // 该请求允许的可写根（Op=OpWrite 时非空）
}

// FileSystem 是 fs 能力 seam 的抽象（DSH FileSystem 对位）。
type FileSystem interface {
	Resolve(ctx context.Context, req Request) (*Resolved, error)
	Mode() Mode
}

// PathDeniedError 结构化错误（DSH FS_SANDBOX_DENIED 对位）。
type PathDeniedError struct {
	Display      string
	Target       string
	Workspace    string
	Op           Op
	Mode         Mode
	AllowedRoots []string
}

func (e *PathDeniedError) Error() string {
	return fmt.Sprintf("路径 %q 在 %s 模式下不允许 %s（workspace=%q, allowed=%v）",
		e.Display, e.Mode, e.Op, e.Workspace, e.AllowedRoots)
}

// NotConfiguredError 表示 ctx 未注入 FileSystem（fail-closed）。
type NotConfiguredError struct{}

func (e *NotConfiguredError) Error() string {
	return "filesystem capability 未注入（fs.FromContext 返回 nil）"
}
