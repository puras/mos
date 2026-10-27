package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/tool"
)

// ls 列目录内容（PI 的 ls 对位）。
//
// 为什么需要：read 只能读文件、grep/glob 只能按内容或模式找——
// "这个目录里有什么"是最基础的一问，没有它，模型只能靠 glob 猜目录结构。
//
// 语义：
//   - 含隐藏项（以 "." 开头，与 PI 一致——隐藏文件常常正是要找的）；
//   - 按名称排序；
//   - 只读：输出即环境回答（审计门白名单内）；
//   - 目录项带 type=dir，模型据此决定要不要进去看。
type Ls struct {
	FS filesystem.FileSystem
}

// NewLs 构造目录列举工具。
func NewLs(fsys filesystem.FileSystem) *Ls { return &Ls{FS: fsys} }

func (Ls) Name() string { return "ls" }

func (Ls) Description() string {
	return "列出目录内容（受工作目录沙箱限制）。参数：path=目录路径（默认工作目录）；" +
		"limit=最多返回多少条目（默认 500）。" +
		"返回按名称排序的条目列表，每项含 name/type（dir 或 file）/size；" +
		"含隐藏项。要看某个文件的内容用 read，要在整棵树里找文件用 glob。"
}

type lsParams struct {
	Path  string `json:"path,omitempty" mos:"desc=目录路径（相对路径以工作目录为锚点），默认工作目录"`
	Limit int    `json:"limit,omitempty" mos:"desc=最多返回多少条目，默认 500"`
}

func (Ls) Parameters() any { return &lsParams{} }

// ConcurrencySafe 实现 tool.ConcurrencySafe：纯只读，可并发。
func (Ls) ConcurrencySafe(json.RawMessage) bool { return true }

// Execute 实现 tool.Tool.Execute。
func (l *Ls) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[lsParams](args)
	if err != nil {
		return nil, err
	}
	if l.FS == nil {
		return nil, &filesystem.NotConfiguredError{}
	}
	res, err := l.FS.Resolve(ctx, filesystem.Request{Path: p.Path, Op: filesystem.OpRead})
	if err != nil {
		return nil, err
	}
	if !isDirPath(res.TargetKey) {
		return nil, fmt.Errorf("%q 不是目录", res.DisplayPath)
	}
	limit := clampLimit(p.Limit, defaultLsLimit, maxLsLimit)

	dirEntries, err := os.ReadDir(res.TargetKey)
	if err != nil {
		return nil, fmt.Errorf("读取目录 %q 失败: %w", res.DisplayPath, err)
	}
	entries := make([]map[string]any, 0, len(dirEntries))
	truncated := false
	for _, de := range dirEntries {
		if len(entries) >= limit {
			truncated = true
			break
		}
		kind := "file"
		var size int64
		switch {
		case de.IsDir():
			kind = "dir"
		case de.Type()&os.ModeSymlink != 0:
			kind = "symlink"
		default:
			if info, err := de.Info(); err == nil {
				size = info.Size()
			}
		}
		entries = append(entries, map[string]any{
			"name": de.Name(),
			"type": kind,
			"size": size,
		})
	}
	return map[string]any{
		"path":      res.DisplayPath,
		"entries":   entries,
		"count":     len(entries),
		"truncated": truncated,
	}, nil
}

// Option 是工具集装配的可选配置（当前只有排除谓词）。
//
// 装配层只给“机制”（怎么排除、排除什么由宿主定），不猜宿主的安全策略。
type Option func(*options)

// options 是 Option 的落点。
type options struct {
	exclude func(path string) bool
}

// WithExclude 给搜索类工具挂上宿主策略的排除谓词：返回 true 的文件不参与
// grep / glob（也不进结果）。
//
// 典型用途：宿主在参数级规则里拒绝 read 敏感文件（.env / 私钥），但 grep
// 是按内容搜的——不挂这个谓词，“凭据不进模型上下文”的保证就被绕过了。
func WithExclude(pred func(path string) bool) Option {
	return func(o *options) { o.exclude = pred }
}

// newOptions 应用选项，返回生效的配置。
func newOptions(opts []Option) *options {
	o := &options{}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	return o
}

// SearchTools 返回受文件能力约束的**只读搜索工具集**（grep / glob / ls）。
//
// 与 FileTools 的分工：FileTools 改世界（write / edit），SearchTools 只读。
// 两者共用同一个 filesystem 能力（同一套 jail 边界）。
//
// opts 可挂宿主策略（当前是 WithExclude）。
//
// fsys 为 nil 时返回空集——不装配文件能力就不给搜索能力（fail-closed：
// 给模型一个必然失败的工具不如不给）。
func SearchTools(fsys filesystem.FileSystem, opts ...Option) []tool.Tool {
	if fsys == nil {
		return nil
	}
	o := newOptions(opts)
	grep := NewGrep(fsys)
	glob := NewGlob(fsys)
	grep.Exclude = o.exclude
	glob.Exclude = o.exclude
	return []tool.Tool{grep, glob, NewLs(fsys)}
}
