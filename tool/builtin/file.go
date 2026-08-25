package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"hegq.net/mos/tool"
	"hegq.net/mos/tool/fs"
)

const defaultReadLimit = 8192

// ReadFile 读取文件内容，支持偏移与长度限制。
type ReadFile struct{}

type readFileParams struct {
	Path   string `json:"path" mos:"desc=文件路径"`
	Offset int    `json:"offset,omitempty" mos:"desc=起始偏移（字节），默认 0"`
	Limit  int    `json:"limit,omitempty" mos:"desc=读取长度（字节），默认 8192"`
}

func (ReadFile) Name() string        { return "read_file" }
func (ReadFile) Description() string { return "读取文件内容，支持指定偏移与长度" }
func (ReadFile) Parameters() any     { return &readFileParams{} }

// Execute 读取文件（file-tool-jail Task 15）：路径先经 fs capability 解析，
// 越界返回 *fs.PathDeniedError；ctx 未注入 fs 时 fail-closed 返回 *fs.NotConfiguredError。
func (ReadFile) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[readFileParams](args)
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
	r, err := fsys.Resolve(ctx, fs.Request{Path: p.Path, Op: fs.OpRead})
	if err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	f, err := os.Open(r.TargetKey)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if p.Offset > 0 {
		if _, err := f.Seek(int64(p.Offset), io.SeekStart); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, limit)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return nil, err
	}
	content := string(buf[:n])
	return map[string]any{
		"path":    p.Path,
		"size":    info.Size(),
		"offset":  p.Offset,
		"content": content,
	}, nil
}

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
func (WriteFile) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[writeFileParams](args)
	if err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path 不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
		return nil, err
	}
	flag := os.O_CREATE | os.O_WRONLY
	if p.Append {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(p.Path, flag, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := f.WriteString(p.Content)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path":   p.Path,
		"bytes":  n,
		"append": p.Append,
	}, nil
}

var _ tool.Tool = ReadFile{}
var _ tool.Tool = WriteFile{}
