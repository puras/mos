package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/puras/mos/tool"
	fs "github.com/puras/mos/filesystem"
)

// WriteFile 写入或追加文件内容。
type WriteFile struct{}

type writeFileParams struct {
	Path    string `json:"path" mos:"desc=文件路径"`
	Content string `json:"content" mos:"desc=要写入的内容"`
	Append  bool   `json:"append,omitempty" mos:"desc=是否追加写入，默认覆盖"`
}

func (WriteFile) Name() string { return "write_file" }
func (WriteFile) Description() string {
	return "写入或追加文本到文件（自动创建父目录）"
}
func (WriteFile) Parameters() any { return &writeFileParams{} }

// Execute 写入文件（file-tool-jail Task 16）：路径先经 fs capability 以 OpWrite 解析
// （Lexical + Realpath 双层，防 symlink TOCTOU），越界返回 *fs.PathDeniedError；
// ctx 未注入 fs 时 fail-closed 返回 *fs.NotConfiguredError。
func (WriteFile) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[writeFileParams](args)
	if err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path 不能为空")
	}
	fsys := fs.FromContext(ctx)
	if fsys == nil {
		return nil, &fs.NotConfiguredError{}
	}
	r, err := fsys.Resolve(ctx, fs.Request{Path: p.Path, Op: fs.OpWrite})
	if err != nil {
		return nil, err
	}
	resolved := r.TargetKey
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return nil, err
	}
	flag := os.O_CREATE | os.O_WRONLY
	if p.Append {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(resolved, flag, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := f.WriteString(p.Content)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path":   resolved,
		"bytes":  n,
		"append": p.Append,
	}, nil
}

var _ tool.Tool = WriteFile{}