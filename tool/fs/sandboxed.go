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
//
// 处理顺序：
//  1. 先用 Lstat 区分 symlink 与普通路径。普通路径走 EvalSymlinks → 二次 Lexical。
//  2. symlink 用 Lstat 拿到 link 本身的 inode 不行——必须解析其指向目标。
//     - 目标存在：EvalSymlinks → 二次 Lexical（防 symlink 链路中途逃出）。
//     - 目标不存在（IsNotExist）：EvalSymlinks 失败，但写操作仍会沿 symlink 创建文件，
//     必须从**已存在**的祖先目录向上解析，找到 link 链中第一个真实存在的目录，
//     把 "parent_resolved + basename(link_target)" 当作实体目标再 Lexical 校验，
//     否则 broken symlink → 越界文件 会被静默放行。
func (s *Sandboxed) realpathWrite(req Request, abs, workspace string) (*Resolved, error) {
	lst, lerr := os.Lstat(abs)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			// 全新文件：abs 路径本身不在文件系统），Lexical 已通过且 abs 在 workspace 内即允许。
			return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
		}
		return nil, lerr
	}

	// 实体（EvalSymlinks 后）再走一次 Lexical，捕获 symlink 链中逃出 workspace 的层级。
	checkResolved := func(resolved string) (*Resolved, error) {
		resolvedAbs, err := filepath.Abs(resolved)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(workspace, resolvedAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, s.denied(req, abs, workspace)
		}
		return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
	}

	if lst.Mode()&os.ModeSymlink == 0 {
		// 普通文件/目录：abs 是其本体，EvalSymlinks 解析其上方可能存在的 symlink 组件。
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
			}
			return nil, err
		}
		return checkResolved(resolved)
	}

	// abs 本身是 symlink。读其链接目标，递归解析直到不再是 symlink 或解析失败。
	target, err := os.Readlink(abs)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(abs), target)
	}
	targetStat, err := os.Stat(target)
	if err == nil {
		if targetStat.Mode()&os.ModeSymlink == 0 {
			// 目标是普通文件/目录，直接走二次 Lexical。
			return checkResolved(target)
		}
		// 目标是 symlink：递归解析。
		return s.realpathWrite(req, target, workspace)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	// broken symlink：目标不存在。从 target 向上回溯到第一个存在的祖先，
	// 把 "ancestor + basename(target)" 作为推断实体，再二次 Lexical。
	probe := target
	for {
		parent := filepath.Dir(probe)
		if parent == probe {
			return nil, s.denied(req, abs, workspace)
		}
		if _, err := os.Stat(parent); err == nil {
			return checkResolved(filepath.Join(parent, filepath.Base(probe)))
		}
		probe = parent
	}
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
