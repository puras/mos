package builtin

import (
	"context"
	"encoding/json"
	iofs "io/fs"
	"path/filepath"
	"strings"

	"hegq.net/mos/tool"
	fs "hegq.net/mos/filesystem"
)

const (
	defaultMaxDepth = 4
	defaultMaxFiles = 50
)

// Glob 按文件名模式在目录树中搜索文件（glob 通配符）。
type Glob struct{}

type globParams struct {
	Root     string `json:"root,omitempty" mos:"desc=起始目录，默认当前目录"`
	Pattern  string `json:"pattern" mos:"desc=文件名匹配模式，支持 * 通配符，如 *.go"`
	MaxDepth int    `json:"max_depth,omitempty" mos:"desc=最大遍历深度，默认 4"`
	MaxFiles int    `json:"max_files,omitempty" mos:"desc=返回文件数上限，默认 50"`
}

func (Glob) Name() string        { return "glob" }
func (Glob) Description() string { return "按文件名通配模式在目录树中搜索文件" }
func (Glob) Parameters() any     { return &globParams{} }

// resolveRoot 解析搜索根（file-tool-jail Task 17/18）：Root 空时默认 workspace root，
// 非空时以 OpList 解析 jail。ctx 未注入 fs 时 fail-closed 返回 *fs.NotConfiguredError。
func resolveRoot(ctx context.Context, p string) (string, error) {
	fsys := fs.FromContext(ctx)
	if fsys == nil {
		return "", &fs.NotConfiguredError{}
	}
	if p == "" {
		p = "."
	}
	r, err := fsys.Resolve(ctx, fs.Request{Path: p, Op: fs.OpList})
	if err != nil {
		return "", err
	}
	return r.TargetKey, nil
}

func (Glob) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[globParams](args)
	if err != nil {
		return nil, err
	}
	root, err := resolveRoot(ctx, p.Root)
	if err != nil {
		return nil, err
	}
	maxDepth := p.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	maxFiles := p.MaxFiles
	if maxFiles <= 0 {
		maxFiles = defaultMaxFiles
	}

	var matches []string
	walkErr := filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过无权限/不存在的子目录
		}
		if d.IsDir() {
			rel, rerr := filepath.Rel(root, path)
			if rerr == nil && rel != "." && strings.Count(rel, string(filepath.Separator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		ok, merr := filepath.Match(p.Pattern, d.Name())
		if merr != nil {
			return nil
		}
		if ok {
			matches = append(matches, path)
			if len(matches) >= maxFiles {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil && !isSkipAll(walkErr) {
		return nil, walkErr
	}
	return map[string]any{
		"root":    root,
		"pattern": p.Pattern,
		"matches": matches,
		"count":   len(matches),
	}, nil
}

// isSkipAll 判断 WalkDir 是否因 SkipAll 提前结束。
func isSkipAll(err error) bool {
	return err == filepath.SkipAll
}

var _ tool.Tool = Glob{}