package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 路径范围（S5f-2b）：默认派生、收窄校验、全开判定。
//
// 语义（fail-closed 默认）：
//   - Runner 有**默认范围**：Local.DefaultScope，nil 时从 WorkDir 派生
//     （读 = 写 = WorkDir）。**忘了配置 = 只有工作区**，不是全盘——"忘记
//     声明"这个最常见的错误，后果必须是最小权限而不是整盘开放；
//   - Request.PathScope 只能**收窄**：必须是默认范围的子集。想放宽，在
//     **装配期**声明更宽的 DefaultScope（危险写在脸上，审查看得见），
//     运行期不许放宽——放宽是宿主的政策，不是调用方（模型）的权力；
//   - 写根包含 "/" = **全开**：没有要强制的边界，直接执行，不做样子。
//     这是显式逃生门（容器场景："/" 是一次性容器的根）。

// deriveScope 从工作目录派生默认范围：读 = 写 = WorkDir。
func deriveScope(workDir string) (*PathScope, error) {
	dir := workDir
	if dir == "" {
		// 与命令的工作目录一致：WorkDir 为空 = 进程当前目录。
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("sandbox: 派生默认路径范围失败（无 WorkDir 且取不到当前目录）: %w", err)
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("sandbox: 派生默认路径范围失败: %w", err)
	}
	abs = filepath.Clean(abs)
	return &PathScope{ReadRoots: []string{abs}, WriteRoots: []string{abs}}, nil
}

// effectiveScope 解析一次调用的有效范围：Request 显式给的优先（但只能收窄），
// 否则用 Runner 默认。
func (s *Local) effectiveScope(reqScope *PathScope) (*PathScope, error) {
	def := s.DefaultScope
	if def == nil {
		d, err := deriveScope(s.WorkDir)
		if err != nil {
			return nil, err
		}
		def = d
	}
	if err := validateScope(def); err != nil {
		return nil, fmt.Errorf("sandbox: 默认路径范围不合法: %w", err)
	}
	if reqScope == nil {
		return def, nil
	}
	if err := validateScope(reqScope); err != nil {
		return nil, fmt.Errorf("sandbox: 路径范围不合法: %w", err)
	}
	// 只能收窄：请求的每个根都必须落在默认范围的某个根之内。
	if err := scopeWithin(reqScope, def); err != nil {
		return nil, fmt.Errorf("sandbox: 路径范围不能比默认更宽（放宽请在装配期声明 DefaultScope）: %w", err)
	}
	return reqScope, nil
}

// validateScope 校验根的形态：必须是非空、绝对、干净的路径。
func validateScope(scope *PathScope) error {
	if scope == nil {
		return errors.New("范围为 nil")
	}
	for _, r := range [][]string{scope.ReadRoots, scope.WriteRoots} {
		for _, root := range r {
			if strings.TrimSpace(root) == "" {
				return errors.New("存在空根")
			}
			if !filepath.IsAbs(root) {
				return fmt.Errorf("根 %q 不是绝对路径", root)
			}
		}
	}
	return nil
}

// scopeWithin 判定 sub 是否为 sup 的子集（只能收窄）。
func scopeWithin(sub, sup *PathScope) error {
	if err := rootsWithin(sub.WriteRoots, sup.WriteRoots); err != nil {
		return fmt.Errorf("写根越界: %w", err)
	}
	if err := rootsWithin(sub.ReadRoots, sup.ReadRoots); err != nil {
		return fmt.Errorf("读根越界: %w", err)
	}
	return nil
}

// rootsWithin 要求 subs 的每个根都落在 sups 的某个根之内。
func rootsWithin(subs, sups []string) error {
	for _, sub := range subs {
		c := filepath.Clean(sub)
		ok := false
		for _, sup := range sups {
			if pathWithin(filepath.Clean(sup), c) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%q 不在允许的根内", sub)
		}
	}
	return nil
}

// pathWithin 判定 p 是否在 root 之内（含相等）。"/" 包容一切。
func pathWithin(root, p string) bool {
	if root == "/" || root == "." {
		return true
	}
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// writeOpen 判定写是否完全放开（写根含 "/"）——此时没有要强制的边界。
// 这是**显式**逃生门：只有宿主在配置里写下 "/" 才会走到这里。
func writeOpen(scope *PathScope) bool {
	if scope == nil {
		return false
	}
	for _, r := range scope.WriteRoots {
		if filepath.Clean(r) == "/" {
			return true
		}
	}
	return false
}
