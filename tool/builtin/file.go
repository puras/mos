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
//
// 32KB：对齐 PI（2000 行或 256KB 封顶）与 DSH（部署可配 maxLimit）的量级——
// 上限太小会让每个中等文件都变成多轮往返。
const defaultReadLimit = 32 * 1024

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
	Offset int    `json:"offset,omitempty" mos:"desc=起始偏移（字节），默认 0；续读时直接用上一段结果里的 next_offset"`
	Limit  int    `json:"limit,omitempty" mos:"desc=读取长度（字节），默认 32768；文件较大时调大它可一次读完"`
}

func (ReadFile) Name() string { return "read_file" }

func (ReadFile) Description() string {
	// 描述是模型唯一的使用指南（MoS 暂未提供“工具自带的系统提示贡献”通道），
	// 所以把分段读的正解写在这里：先试一次读完，不够再**并行**补齐。
	return "读取文件内容（受工作目录沙箱限制）。" +
		"文件较大时优先调大 limit 一次读完；" +
		"确需分段时，可在同一条消息里一次发出多个不同 offset 的 read_file 调用并行读取剩余部分" +
		"（结果里的 size 是文件总字节数，next_offset 是下一段的起始偏移，0 表示已读完）。"
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
	// ReadFull：读满 limit 字节或撞到文件末尾（返回已读字节数与 EOF/ErrUnexpectedEOF）。
	// 单次 Read 可能只拿到一部分，模型无法判断“到底读了多少”，续读偏移会算错。
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	// next_offset：下一段的起始偏移；0 表示已读完（对齐 PI 的
	// "Use offset=N to continue" 提示——模型不必自己拿 size 去减）。
	next := 0
	if int64(p.Offset)+int64(n) < info.Size() {
		next = p.Offset + n
	}
	return map[string]any{
		"path":        res.DisplayPath,
		"size":        info.Size(),
		"offset":      p.Offset,
		"next_offset": next,
		"content":     string(buf[:n]),
	}, nil
}

var _ tool.Tool = (*ReadFile)(nil)

// ConcurrencySafe 声明读操作可与同批其他调用并发（只读、无共享状态）。
// 写工具不声明——写操作保持独占（屏障），避免两个写互相穿插。
func (r *ReadFile) ConcurrencySafe(_ json.RawMessage) bool { return true }

var _ tool.ConcurrencySafe = (*ReadFile)(nil)

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
	// 结果里带 created / replaced_bytes：模型据此知道自己是新建还是覆盖了已有
	// 内容（DSH 的 write 区分 Created/Updated，PI 的 edit 报告改了几处）——
	// 没有这个信息，模型只能读回来确认，反复重写就容易空转。
	return "写入或追加文本到文件（自动创建父目录；受工作目录沙箱限制）。" +
		"覆盖已有文件前建议先 read_file 确认——结果里的 created 表示本次是否新建，" +
		"replaced_bytes 表示覆盖模式下被丢弃的原有字节数。"
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
	// 写入前的状态：新建还是覆盖？覆盖掉了多少原有内容？
	// 模型需要这个才知道自己有没有冲掉已有内容（否则只能读回来确认，容易空转）。
	created := true
	var oldSize int64
	if info, err := os.Stat(target); err == nil {
		created = false
		oldSize = info.Size()
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
	// replaced_bytes：覆盖模式（O_TRUNC）下原有内容全部被丢弃；追加模式不丢弃任何内容。
	replaced := int64(0)
	if !p.Append {
		replaced = oldSize
	}
	return map[string]any{
		"path":           res.DisplayPath,
		"bytes":          n,
		"append":         p.Append,
		"created":        created,
		"replaced_bytes": replaced,
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
