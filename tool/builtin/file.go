package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/tool"
)

// defaultReadLimit 是 read_file 的默认读取上限（字节）。
const defaultReadLimit = 8192

// ReadFile 读取文件内容，支持偏移与长度限制。
//
// 路径先经 filesystem 能力解析（Lexical + Realpath 双层），越界返回
// *filesystem.PathDeniedError；未装配文件能力时返回 *filesystem.NotConfiguredError
// （fail-closed：拒绝而不是放行）。
type ReadFile struct {
	FS filesystem.FileSystem
}

// NewReadFile 构造读文件工具。
func NewReadFile(fsys filesystem.FileSystem) *ReadFile { return &ReadFile{FS: fsys} }

type readFileParams struct {
	Path   string `json:"path" mos:"desc=文件路径（相对路径以工作目录为锚点）"`
	Offset int    `json:"offset,omitempty" mos:"desc=起始偏移（字节），默认 0"`
	Limit  int    `json:"limit,omitempty" mos:"desc=读取长度（字节），默认 8192"`
}

func (ReadFile) Name() string { return "read_file" }

func (ReadFile) Description() string {
	return "读取文件内容，支持指定偏移与长度（受工作目录沙箱限制）"
}

func (ReadFile) Parameters() any { return &readFileParams{} }

// Execute 实现 tool.Tool.Execute。
func (r *ReadFile) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[readFileParams](args)
	if err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path 不能为空")
	}
	if r.FS == nil {
		return nil, &filesystem.NotConfiguredError{}
	}
	res, err := r.FS.Resolve(ctx, filesystem.Request{Path: p.Path, Op: filesystem.OpRead})
	if err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	f, err := os.Open(res.TargetKey)
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
	return map[string]any{
		"path":    res.DisplayPath,
		"size":    info.Size(),
		"offset":  p.Offset,
		"content": string(buf[:n]),
	}, nil
}

var _ tool.Tool = (*ReadFile)(nil)

// WriteFile 写入或追加文件内容。
//
// 路径先经 filesystem 能力以 OpWrite 解析（Lexical + Realpath 双层，防 symlink
// 逃逸与经链接创建），越界返回 *filesystem.PathDeniedError。
type WriteFile struct {
	FS filesystem.FileSystem
}

// NewWriteFile 构造写文件工具。
func NewWriteFile(fsys filesystem.FileSystem) *WriteFile { return &WriteFile{FS: fsys} }

type writeFileParams struct {
	Path    string `json:"path" mos:"desc=文件路径（相对路径以工作目录为锚点）"`
	Content string `json:"content" mos:"desc=要写入的内容"`
	Append  bool   `json:"append,omitempty" mos:"desc=是否追加写入，默认覆盖"`
}

func (WriteFile) Name() string { return "write_file" }

func (WriteFile) Description() string {
	return "写入或追加文本到文件（自动创建父目录；受工作目录沙箱限制）"
}

func (WriteFile) Parameters() any { return &writeFileParams{} }

// Execute 实现 tool.Tool.Execute。
func (w *WriteFile) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[writeFileParams](args)
	if err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path 不能为空")
	}
	if w.FS == nil {
		return nil, &filesystem.NotConfiguredError{}
	}
	res, err := w.FS.Resolve(ctx, filesystem.Request{Path: p.Path, Op: filesystem.OpWrite})
	if err != nil {
		return nil, err
	}
	target := res.TargetKey
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	flag := os.O_CREATE | os.O_WRONLY
	if p.Append {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(target, flag, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := f.WriteString(p.Content)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path":   res.DisplayPath,
		"bytes":  n,
		"append": p.Append,
	}, nil
}

var _ tool.Tool = (*WriteFile)(nil)

// FileTools 返回受文件能力约束的工具集（读 + 写）。
//
// 宿主没装配文件能力时可以不调用本函数——不给模型这个能力，比给它一个
// 必然失败的工具更干净。
func FileTools(fsys filesystem.FileSystem) []tool.Tool {
	return []tool.Tool{NewReadFile(fsys), NewWriteFile(fsys)}
}
