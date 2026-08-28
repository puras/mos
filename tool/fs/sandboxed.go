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
// 读写都 jail → saas-jailed（DSH read-only / SaaS 默认）；
// 只 jail 其中一侧 → local-jailed（DSH workspace-write：写 fenced，读放开）；
// 都不 jail → danger-full（此实例本不该被装配，assistant.buildFileSystem 会选 Local）。
func (s *Sandboxed) Mode() Mode {
	switch {
	case s.Policy.JailReads && s.Policy.JailWrites:
		return ModeSaaSJailed
	case s.Policy.JailReads || s.Policy.JailWrites:
		return ModeLocalJailed
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
		return nil, s.denied(ctx, req, abs, workspace)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, s.denied(ctx, req, abs, workspace)
	}

	if req.Op != OpWrite {
		return s.allowed(ctx, req, abs, workspace)
	}

	// Op=Write：Realpath 阶段（DSH fsio.ts:146-194）。
	// 实体比对以 EvalSymlinks 后的 workspace 为基准：workspace 自身可能位于
	// 符号链接下（如 macOS 的 /tmp → /private/tmp），否则真实祖先会被误判为越界。
	wsEntity := workspace
	if r, err := filepath.EvalSymlinks(workspace); err == nil {
		wsEntity = r
	}
	return s.realpathWrite(ctx, req, abs, workspace, wsEntity)
}

// realpathWrite 实现 Op=Write 的实体比对，防 symlink TOCTOU 与“经越界 symlink 组件
// 创建新文件”两类逃逸。
//
// 三条路径：
//  1. 目标不存在（Lstat NotExist）：中间组件可能指向 workspace 外（如 ws/escape → /etc，
//     写 ws/escape/new.txt 时 Lstat 报 NotExist 但实际会经 escape 落到 /etc）。
//     必须走 resolveNewPath 验证最深存在祖先的解析结果。
//  2. 目标存在且非 symlink：EvalSymlinks 解析其上方组件；整链失败为 NotExist 时
//     同样落 resolveNewPath（存在前缀本身可能是越界 symlink）。
//  3. 目标本身是 symlink：读链接目标递归（带深度上限防环），终止条件交由 1/2 分支处理。
const maxSymlinkDepth = 32

func (s *Sandboxed) realpathWrite(ctx context.Context, req Request, abs, workspace, wsEntity string) (*Resolved, error) {
	return s.realpathWriteN(ctx, req, abs, workspace, wsEntity, 0)
}

func (s *Sandboxed) realpathWriteN(ctx context.Context, req Request, abs, workspace, wsEntity string, depth int) (*Resolved, error) {
	if depth > maxSymlinkDepth {
		// 链接环：内核 open 会报 ELOOP，这里提前拒绝避免无限递归。
		return nil, s.denied(ctx, req, abs, workspace)
	}
	lst, lerr := os.Lstat(abs)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			// 叶子不存在≠安全：已有中间组件可能把写入重定向到 workspace 外。
			return s.resolveNewPath(ctx, req, abs, workspace, wsEntity)
		}
		return nil, lerr
	}

	if lst.Mode()&os.ModeSymlink == 0 {
		// 普通文件/目录：abs 是其本体，EvalSymlinks 解析其上方可能存在的 symlink 组件。
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			if os.IsNotExist(err) {
				// 整链有缺失段：存在前缀仍可能含越界 symlink，不能盲放行。
				return s.resolveNewPath(ctx, req, abs, workspace, wsEntity)
			}
			return nil, err
		}
		return s.checkEntity(ctx, req, abs, workspace, wsEntity, resolved)
	}

	// abs 本身是 symlink：解析目标继续判定（Readlink 不跟随，深度计数防环）。
	target, err := os.Readlink(abs)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(abs), target)
	}
	return s.realpathWriteN(ctx, req, target, workspace, wsEntity, depth+1)
}

// resolveNewPath 判定“待创建”路径的实际落点：沿 abs 向上找最深**存在**的祖先，
// EvalSymlinks 解析出真实位置，再把剩余后缀拼回做 Lexical 校验。
// 缺失组件不可能成为 symlink 后门，故只需验证存在前缀；任一层失败即拒绝。
func (s *Sandboxed) resolveNewPath(ctx context.Context, req Request, abs, workspace, wsEntity string) (*Resolved, error) {
	suffix := ""
	probe := abs
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			return s.checkEntity(ctx, req, abs, workspace, wsEntity, filepath.Join(resolved, suffix))
		}
		if !os.IsNotExist(err) {
			return nil, err // ELOOP 等：fail-closed
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			// 到达根仍无存在祖先：不可达路径，防御性拒绝。
			return nil, s.denied(ctx, req, abs, workspace)
		}
		suffix = filepath.Join(filepath.Base(probe), suffix)
		probe = parent
	}
}

// checkEntity 对 EvalSymlinks 解析后的实体路径做二次 Lexical 校验：
// 实体在 wsEntity（workspace 实体位置）内则放行，否则拒绝。
func (s *Sandboxed) checkEntity(ctx context.Context, req Request, abs, workspace, wsEntity, entity string) (*Resolved, error) {
	entityAbs, err := filepath.Abs(entity)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(wsEntity, entityAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, s.denied(ctx, req, abs, workspace)
	}
	return s.allowed(ctx, req, abs, workspace)
}

// denied 构造结构化 PathDeniedError，并发 fs/resolve 审计事件（decision=denied）。
// 所有拒绝路径的唯一出口，保证"被拒必留痕"。
func (s *Sandboxed) denied(ctx context.Context, req Request, abs, workspace string) error {
	err := &PathDeniedError{
		Display: req.Path, Target: abs, Workspace: workspace,
		Op: req.Op, Mode: s.Mode(),
		AllowedRoots: WritableRoots(s.Policy),
	}
	emitResolve(ctx, req, s.Mode(), abs, workspace, "denied", err.Error())
	return err
}

// allowed 构造放行结果，并发 fs/resolve 审计事件（decision=allowed）。
// 所有放行路径的唯一出口，与 denied 对称。
func (s *Sandboxed) allowed(ctx context.Context, req Request, abs, workspace string) (*Resolved, error) {
	emitResolve(ctx, req, s.Mode(), abs, workspace, "allowed", "")
	return &Resolved{DisplayPath: abs, TargetKey: abs}, nil
}

var _ FileSystem = (*Sandboxed)(nil)
