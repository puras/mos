package assistant

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// checkFile 校验文件路径不超出工作目录。
func (p *DefaultPolicy) checkFile(argsJSON, field string) error {
	if p.WorkDir == "" {
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
	return withinDir(p.WorkDir, raw)
}

// withinDir 判断 target 路径是否位于 root 目录内。
func withinDir(root, target string) error {
	r, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	t := target
	if !filepath.IsAbs(t) {
		t = filepath.Join(r, t)
	}
	t, err = filepath.Abs(t)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r, t)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %q 超出工作目录 %q", target, r)
	}
	return nil
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
