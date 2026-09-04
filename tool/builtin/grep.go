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

	"github.com/puras/mos/tool"
)

const grepMaxFileSize = 1 << 20 // 单文件扫描上限 1MB

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
func (Grep) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[grepParams](args)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return nil, fmt.Errorf("无效正则: %w", err)
	}
	root, err := resolveRoot(ctx, p.Root)
	if err != nil {
		return nil, err
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

var _ tool.Tool = Grep{}