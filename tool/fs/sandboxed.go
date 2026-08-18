package fs

import (
	"context"
	"path/filepath"
	"strings"
)

// Sandboxed 是 FileSystem 的 jail 实现（与 DSH SandboxedFileSystem 对位）。
// Resolve 分两阶段：Lexical（路径前缀比对）+ Realpath（实体 inode 比对，本 task 仅 Op=Write；Op=Read 走 Lexical 即返回）。
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

// Resolve 实现 FileSystem.Resolve（Lexical 阶段；Op=Write 的 Realpath 在下个 task 接入）。
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
		return nil, &PathDeniedError{
			Display: req.Path, Target: abs, Workspace: workspace,
			Op: req.Op, Mode: s.Mode(),
		}
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, &PathDeniedError{
			Display: req.Path, Target: abs, Workspace: workspace,
			Op: req.Op, Mode: s.Mode(),
			AllowedRoots: WritableRoots(s.Policy),
		}
	}
	return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
}

var _ FileSystem = (*Sandboxed)(nil)
