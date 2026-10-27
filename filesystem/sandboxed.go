package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// maxSymlinkDepth 限制 symlink 解析深度（防环；内核 open 也会报 ELOOP，这里提前拒）。
const maxSymlinkDepth = 32

// Sandboxed 是 FileSystem 的 jail 实现：读与写都在宿主声明的根内，越界返回
// *PathDeniedError。
//
// 校验分两层：
//   - Lexical：规范化后的路径必须落在某个允许根的子树内（挡 ../ 与绝对路径越界）；
//   - Realpath：解析符号链接后的**实体路径**必须仍落在允许根的实体内
//     （挡 root/link → /etc 这类经链接逃逸，以及 root/escape/new.txt 这类经链接创建）。
//
// 读同样走 Realpath——只做词法校验的话，根内一个指向 /etc/passwd 的链接就能
// 把内容读出边界，那不是 jail。
type Sandboxed struct {
	// Policy 是静态根策略。与 Roots 二选一（Roots 非 nil 时优先）。
	Policy *FileAccessPolicy
	// Roots 是**根来源**（可选）。非 nil 时优先于 Policy——运行期可变的根
	// 集合（*RootSet）走这里：宿主在会话中授权新目录后，无需重建智能体。
	//
	// 每次 Resolve 都现读一次来源（拿一份快照），所以加根对**后续调用**
	// 立即生效（同一轮内也生效）——边界是每次调用现判的，不是冻结的。
	Roots RootProvider
}

// NewSandboxed 构造 jail 实现。Policy 为 nil 或对应根集合为空时，
// 该类操作一律被拒绝（fail-closed）——没有根就没有边界，不是"等于放开"。
func NewSandboxed(p *FileAccessPolicy) *Sandboxed {
	return &Sandboxed{Policy: p}
}

// NewSandboxedWithRoots 用**根来源**构造 jail 实现（运行期可变的根集合走这里）。
//
// rp 为 nil 时一律拒绝（fail-closed）。
func NewSandboxedWithRoots(rp RootProvider) *Sandboxed {
	return &Sandboxed{Roots: rp}
}

var _ FileSystem = (*Sandboxed)(nil)

// rootsFor 取本次请求适用的根集合（快照）。
func (s *Sandboxed) rootsFor(op Op) []string {
	if s.Roots != nil {
		return s.Roots.RootsFor(op)
	}
	if s.Policy != nil {
		return s.Policy.RootsFor(op)
	}
	return nil
}

// anchorFor 取相对路径的锚点：显式锚点优先，否则用该类操作的第一个根。
func (s *Sandboxed) anchorFor(op Op, roots []string) string {
	if s.Roots != nil {
		if ap, ok := s.Roots.(AnchorProvider); ok {
			if a := ap.Anchor(); a != "" {
				return a
			}
		}
	}
	if s.Roots == nil && s.Policy != nil && s.Policy.Anchor != "" {
		return normRoot(s.Policy.Anchor)
	}
	if len(roots) > 0 {
		return roots[0]
	}
	return ""
}

// Resolve 实现 FileSystem.Resolve。
func (s *Sandboxed) Resolve(_ context.Context, req Request) (*Resolved, error) {
	roots := s.rootsFor(req.Op)
	if len(roots) == 0 {
		// 未声明允许的根：拒绝，而不是没有边界。
		return nil, s.denied(req, req.Path, roots)
	}

	// ① Lexical：相对路径以锚点为落点（默认 = 第一个根）；规范化后必须落在
	// 某个允许根内。
	abs := req.Path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.anchorFor(req.Op, roots), abs)
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return nil, s.denied(req, req.Path, roots)
	}
	if !withinAny(roots, abs) {
		return nil, s.denied(req, abs, roots)
	}

	// ② Realpath：解析实体路径，实体仍须落在允许根的实体内。
	entity, err := resolveEntity(abs, 0)
	if err != nil {
		if errors.Is(err, errJailEscape) {
			return nil, s.denied(req, abs, roots)
		}
		return nil, err
	}
	if !withinAnyEntity(roots, entity) {
		return nil, s.denied(req, abs, roots)
	}
	return &Resolved{
		DisplayPath:  abs,
		TargetKey:    entity,
		AllowedRoots: roots,
	}, nil
}

// denied 是所有拒绝路径的唯一出口：构造结构化错误，保证「被拒必留痕」。
func (s *Sandboxed) denied(req Request, target string, roots []string) error {
	return &PathDeniedError{
		Display:      req.Path,
		Target:       target,
		Op:           req.Op,
		AllowedRoots: roots,
	}
}

// errJailEscape 表示路径无法安全解析（链接环、不可达）：一律当作越界拒绝。
var errJailEscape = errors.New("路径无法安全解析")

// resolveEntity 解析路径的实体位置（跟随符号链接），返回可用于打开的绝对路径。
//
// 三条路径：
//  1. 目标不存在（Lstat NotExist）：中间组件可能把访问重定向到根外
//     （如 ws/escape → /etc，读 ws/escape/new.txt 时叶子不存在但实际落在 /etc）。
//     必须走 resolveNewPath 验证最深存在祖先的解析结果。
//  2. 目标存在且非 symlink：EvalSymlinks 解析其上方组件；整链有缺失段时
//     同样落 resolveNewPath（存在前缀本身可能是越界 symlink）。
//  3. 目标本身是 symlink：读链接目标递归（深度上限防环），终止条件交由 1/2 分支。
func resolveEntity(abs string, depth int) (string, error) {
	if depth > maxSymlinkDepth {
		// 链接环：提前拒绝，避免无限递归。
		return "", errJailEscape
	}
	lst, lerr := os.Lstat(abs)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return resolveNewPath(abs)
		}
		return "", lerr
	}
	if lst.Mode()&os.ModeSymlink == 0 {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return resolveNewPath(abs)
			}
			return "", err
		}
		return resolved, nil
	}
	// 目标本身是 symlink：解析其指向，继续判定（Readlink 不跟随，深度计数防环）。
	target, err := os.Readlink(abs)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(abs), target)
	}
	return resolveEntity(target, depth+1)
}

// resolveNewPath 判定「待创建」路径的实际落点：沿 abs 向上找最深**存在**的祖先，
// EvalSymlinks 解析出真实位置，再把剩余后缀拼回。缺失组件不可能是 symlink 后门，
// 故只需验证存在前缀；任一层失败即拒绝。
func resolveNewPath(abs string) (string, error) {
	suffix := ""
	probe := abs
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		if !os.IsNotExist(err) {
			return "", err // ELOOP 等：fail-closed
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			// 到根仍无存在祖先：不可达路径。
			return "", errJailEscape
		}
		suffix = filepath.Join(filepath.Base(probe), suffix)
		probe = parent
	}
}

// withinAny 判断路径是否落在任一根的子树内（词法）。
func withinAny(roots []string, target string) bool {
	for _, r := range roots {
		if underRoot(r, target) {
			return true
		}
	}
	return false
}

// withinAnyEntity 判断实体路径是否落在任一根的**实体**子树内。
// 根本身可能位于符号链接之下（如 macOS 的 /tmp → /private/tmp），
// 否则真实祖先会被误判为越界。
func withinAnyEntity(roots []string, entity string) bool {
	abs, err := filepath.Abs(entity)
	if err != nil {
		return false
	}
	for _, r := range roots {
		entityRoot := r
		if resolved, err := filepath.EvalSymlinks(r); err == nil {
			entityRoot = resolved
		}
		if underRoot(entityRoot, abs) {
			return true
		}
	}
	return false
}
