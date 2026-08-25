package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"hegq.net/mos/tool"
	"hegq.net/mos/tool/fs"
)

const (
	defaultMaxDepth = 4
	defaultMaxFiles = 50
	grepMaxFileSize = 1 << 20 // 单文件扫描上限 1MB
)

// FindFiles 按文件名模式搜索文件。
type FindFiles struct{}

type findParams struct {
	Root     string `json:"root,omitempty" mos:"desc=起始目录，默认当前目录"`
	Pattern  string `json:"pattern" mos:"desc=文件名匹配模式，支持 * 通配符，如 *.go"`
	MaxDepth int    `json:"max_depth,omitempty" mos:"desc=最大遍历深度，默认 4"`
	MaxFiles int    `json:"max_files,omitempty" mos:"desc=返回文件数上限，默认 50"`
}

func (FindFiles) Name() string        { return "find_files" }
func (FindFiles) Description() string { return "按文件名通配模式在目录树中搜索文件" }
func (FindFiles) Parameters() any     { return &findParams{} }

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

func (FindFiles) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[findParams](args)
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

// Grep 在目录树中按正则搜索文件内容。
type Grep struct{}

type grepParams struct {
	Root       string `json:"root,omitempty" mos:"desc=起始目录，默认当前目录"`
	Pattern    string `json:"pattern" mos:"desc=正则表达式"`
	Glob       string `json:"glob,omitempty" mos:"desc=文件过滤模式，如 *.go"`
	MaxMatches int    `json:"max_matches,omitempty" mos:"desc=匹配结果上限，默认 50"`
}

func (Grep) Name() string        { return "grep" }
func (Grep) Description() string { return "按正则表达式在目录树中搜索文件内容" }
func (Grep) Parameters() any     { return &grepParams{} }
func (Grep) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[grepParams](args)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return nil, fmt.Errorf("无效正则: %w", err)
	}
	root := p.Root
	if root == "" {
		root = "."
	}
	maxMatches := p.MaxMatches
	if maxMatches <= 0 {
		maxMatches = defaultMaxFiles
	}

	var results []string
	count := 0
	walkErr := filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if p.Glob != "" {
			if ok, _ := filepath.Match(p.Glob, d.Name()); !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > grepMaxFileSize {
			return nil
		}
		lines, err := grepFile(path, re)
		if err != nil {
			return nil
		}
		for _, line := range lines {
			results = append(results, fmt.Sprintf("%s:%s:%s", path, line[0], line[1]))
			count++
			if count >= maxMatches {
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
		"matches": results,
		"count":   count,
	}, nil
}

// grepFile 返回文件中匹配行（行号, 内容）。
func grepFile(path string, re *regexp.Regexp) ([][2]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out [][2]string
	scanner := bufio.NewScanner(io.LimitReader(f, grepMaxFileSize))
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if re.MatchString(line) {
			if len(line) > 300 {
				line = line[:300] + "..."
			}
			out = append(out, [2]string{fmt.Sprintf("%d", lineNo), line})
		}
	}
	return out, scanner.Err()
}

// isSkipAll 判断 WalkDir 是否因 SkipAll 提前结束。
func isSkipAll(err error) bool {
	return err == filepath.SkipAll
}

var _ tool.Tool = FindFiles{}
var _ tool.Tool = Grep{}
