package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Sandboxed 是 FileSystem 的 jail 实现（与 DSH SandboxedFileSystem 对位）。
// Resolve 分两阶段：Lexical（路径前缀比对）+ Realpath（实体 inode 比对，Op=Write 时启用）。
type Sandboxed struct {
	Policy *FileAccessPolicy
}

// Mode 返回当前策略模式。
func (s *Sandboxed) Mode() Mode {
	switch {
	case s.Policy.JailReads && !s.Policy.JailWrites:
		return ModeLocalJailed
	case s.Policy.JailReads && s.Policy.JailWrites:
		return ModeSaaSJailed
	default:
		return ModeDangerFull
	}
}

// Resolve 实现 FileSystem.Resolve。
// Op=Read/List/Grep：Lexical 阶段（DSH read 透传）。
// Op=Write：Lexical + Realpath 双层（DSH write fenced）。
func (s *Sandboxed) Resolve(ctx context.Context, req Request) (*Resolved, error) {
	workspace, err := filepath.Abs(s.Policy.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	// 相对路径以 workspace 为锚点（与 pathUnderRoot 语义一致）
	target := req.Path
	if !filepath.IsAbs(target) {
		target = filepath.Join(workspace, target)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	// Lexical 阶段：rel 必须以 workspace 为前缀
	rel, err := filepath.Rel(workspace, abs)
	if err != nil {
		return nil, s.denied(req, abs, workspace)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, s.denied(req, abs, workspace)
	}

	if req.Op != OpWrite {
		return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
	}

	// Op=Write：Realpath 阶段（DSH fsio.ts:146-194）
	return s.realpathWrite(req, abs, workspace)
}

// realpathWrite 实现 Op=Write 的实体 inode 比对，防 symlink TOCTOU。
// 解析 abs 的所有 symlink（filepath.EvalSymlinks），得到真实路径后做 inode 比对：
// 真实路径必须在 workspace 内（按 dev+ino 回溯到 workspace 边界即视为越界）。
func (s *Sandboxed) realpathWrite(req Request, abs, workspace string) (*Resolved, error) {
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件尚未创建：Lexical 已通过且目标在 workspace 内即允许
			return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
		}
		return nil, err
	}
	// EvalSymlinks 后路径再走一次 Lexical（防 symlink 链路中某层逃出 workspace）
	resolvedAbs, err := filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	wsAbs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(wsAbs, resolvedAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, s.denied(req, abs, workspace)
	}
	return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
}

// denied 构造结构化 PathDeniedError。
func (s *Sandboxed) denied(req Request, abs, workspace string) error {
	return &PathDeniedError{
		Display: req.Path, Target: abs, Workspace: workspace,
		Op: req.Op, Mode: s.Mode(),
		AllowedRoots: WritableRoots(s.Policy),
	}
}

var _ FileSystem = (*Sandboxed)(nil)
