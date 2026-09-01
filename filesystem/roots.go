package filesystem

import (
	"os"
	"path/filepath"
)

// FileAccessPolicy 描述 fs 访问策略（assistant 层 FileAccessConfig 的镜像）。
type FileAccessPolicy struct {
	WorkspaceRoot string
	JailReads     bool
	JailWrites    bool
	WritableExtra []string
}

// WritableRoots 返回当前请求允许的可写根（去重、Abs 规范化）。
// DSH writableRoots() 的 Go 翻译；fs sandbox 与 shell policy 共用此函数。
func WritableRoots(p *FileAccessPolicy) []string {
	roots := []string{absOrSelf(p.WorkspaceRoot)}
	if !p.JailWrites {
		return dedupStr(roots) // ModeDangerFull：仅 workspace
	}
	roots = append(roots, "/tmp", absOrSelf(os.TempDir()))
	if len(p.WritableExtra) > 0 {
		for _, e := range p.WritableExtra {
			roots = append(roots, absOrSelf(e))
		}
	}
	return dedupStr(roots)
}

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

func dedupStr(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
