package assistant

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"hegq.net/mos/tool/fs"
)

// checkFile 校验文件路径不超出工作目录。
func (p *DefaultPolicy) checkFile(argsJSON, field string) error {
	if p.FileAccess.WorkspaceRoot == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Errorf("无法解析参数: %w", err)
	}
	raw, ok := args[field].(string)
	if !ok || raw == "" {
		return nil
	}
	return withinDir(p.fileAccessPolicy(), raw)
}

// fileAccessPolicy 把 Config 层 FileAccessConfig 镜像为 fs.FileAccessPolicy。
// 字段一一对应；调用方（policy check / fs seam 装配）需要的是 fs 包类型。
func (p *DefaultPolicy) fileAccessPolicy() *fs.FileAccessPolicy {
	return &fs.FileAccessPolicy{
		WorkspaceRoot: p.FileAccess.WorkspaceRoot,
		JailReads:     p.FileAccess.JailReads,
		JailWrites:    p.FileAccess.JailWrites,
		WritableExtra: p.FileAccess.WritableExtra,
	}
}

// withinDir 判断 target 是否落在任一可写根（workspace + extra + /tmp + tempdir）内。
// 多根支持通过 fs.WritableRoots 提供；fs sandbox 与本策略共用同一根集（防配置漂移）。
func withinDir(p *fs.FileAccessPolicy, target string) error {
	for _, root := range fs.WritableRoots(p) {
		if pathUnderRoot(root, target) {
			return nil
		}
	}
	return fmt.Errorf("路径 %q 超出工作目录 %q", target, p.WorkspaceRoot)
}

// pathUnderRoot 判断 target 是否在 root 子树内（Lexical）。
func pathUnderRoot(root, target string) bool {
	r, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	t := target
	if !filepath.IsAbs(t) {
		t = filepath.Join(r, t)
	}
	t, err = filepath.Abs(t)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(r, t)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isNumeric 判断字符串是否全部由数字组成（排除 chmod 模式位等）。
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
