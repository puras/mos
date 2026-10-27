package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/tool"
)

// read_file 的上限（参照 PI 的 read 工具：DEFAULT_MAX_LINES / DEFAULT_MAX_BYTES）。
const (
	// defaultReadLines 是默认读取行数（PI：2000 行）。
	defaultReadLines = 2000
	// maxReadBytes 是单次返回的字节上限（PI：50KB）。行数与字节**先到先算**。
	maxReadBytes = 50 * 1024
	// maxLineChars 是单行字符上限（DSH 的 maxLineLength 对位）。超长的行单独
	// 截断并标记——minified 文件一行能有几百 KB，只靠总量上限会把预算吃光。
	// （PI 的做法是报错并指向 bash；MoS 还没有 shell 工具，所以用截断。）
	maxLineChars = 2000
)

// ReadFile 读取文件内容，支持偏移与长度限制。
//
// 路径先经 filesystem 能力解析（Lexical + Realpath 双层），越界返回
// *filesystem.PathDeniedError；未装配文件能力时返回 *filesystem.NotConfiguredError
// （fail-closed：拒绝而不是放行）。
type ReadFile struct {
	FS filesystem.FileSystem
	// Gate 是写前观察门（可选，nil = 不记录）。读取成功（或确认不存在）后
	// 记录观察，供 write_file / edit_file 做新鲜度校验（S5h）。
	Gate *ObserveGate
}

// NewReadFile 构造读文件工具。
func NewReadFile(fsys filesystem.FileSystem) *ReadFile { return &ReadFile{FS: fsys} }

// NewReadFileObserved 构造带观察门的读文件工具（S5h：写前观察需要读侧记录）。
func NewReadFileObserved(fsys filesystem.FileSystem, gate *ObserveGate) *ReadFile {
	return &ReadFile{FS: fsys, Gate: gate}
}

type readFileParams struct {
	Path   string `json:"path" mos:"desc=文件路径（相对路径以工作目录为锚点）"`
	Offset int    `json:"offset,omitempty" mos:"desc=起始行号（1 起），默认 1；续读时直接用上一段结果里的 next_offset"`
	Limit  int    `json:"limit,omitempty" mos:"desc=读取行数，默认 2000（另有 50KB 总量上限，先到先算）"`
}

func (ReadFile) Name() string { return "read_file" }

func (ReadFile) Description() string {
	// 跨调用约定（先读后写）走 tool.Guideline 通道（WriteFile.Guideline），
	// 这里只讲单次调用怎么用：分段读的正解是先试一次读完，不够再**并行**补齐。
	return "读取文件内容（受工作目录沙箱限制）。按行寻址：offset 是起始行号（从 1 起），" +
		"limit 是读取行数（默认 2000，另有 50KB 总量上限，先到先算）。" +
		"文件较大时优先调大 limit 一次读完；" +
		"确需分段时，可在同一条消息里一次发出多个不同 offset 的 read_file 调用并行读取剩余部分" +
		"（结果里的 size 是文件总字节数，total_lines 是总行数，next_offset 是下一段的起始行号，0 表示已读完）。"
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

	startLine := p.Offset
	if startLine <= 0 {
		startLine = 1
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultReadLines
	}

	f, err := os.Open(res.TargetKey)
	if err != nil {
		// 文件不存在是一次权威观察（确认了"没有"）：记 absent，模型随后可以
		// 直接 write_file 创建（createIfAbsent 语义）。其余错误不记录。
		if r.Gate != nil && errors.Is(err, fs.ErrNotExist) {
			r.Gate.Record(res.TargetKey, "", false)
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// 读取成功 = 权威观察：记下这个版本（S5h）。
	if r.Gate != nil {
		if v, ok := versionOf(info); ok {
			r.Gate.Record(res.TargetKey, v, true)
		}
	}

	// 逐行扫描：只保留需要的窗口（内存有界），但一路数到文件末尾，
	// 好让回执里的 total_lines 与续读提示里的 "of N" 是真的。
	br := bufio.NewReader(f)
	var (
		window   []string
		total    int
		bytesOut int
		hitBytes bool
	)
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil && rerr != io.EOF {
			return nil, rerr
		}
		if rerr == io.EOF && line == "" {
			break
		}
		total++
		if total >= startLine && len(window) < limit {
			rendered := truncateLongLine(line)
			if bytesOut+len(rendered) > maxReadBytes {
				// 字节上限先到（PI：whichever is hit first）
				hitBytes = true
				break
			}
			bytesOut += len(rendered)
			window = append(window, rendered)
		}
		if rerr == io.EOF {
			break
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("文件 %q 是空的", res.DisplayPath)
	}
	if startLine > total {
		return nil, fmt.Errorf("offset %d 超出文件末尾（文件共 %d 行）", startLine, total)
	}

	firstLine := startLine
	lastLine := startLine + len(window) - 1
	nextOffset := 0
	footer := ""
	switch {
	case lastLine >= total:
		footer = fmt.Sprintf("(End of file - total %d lines)", total)
	case hitBytes:
		nextOffset = lastLine + 1
		footer = fmt.Sprintf("[Showing lines %d-%d of %d (%d KB limit). Use offset=%d to continue.]",
			firstLine, lastLine, total, maxReadBytes/1024, nextOffset)
	case p.Limit > 0 && len(window) == p.Limit:
		// 用户自己限了行数（PI 的第三种提示）
		remaining := total - lastLine
		nextOffset = lastLine + 1
		footer = fmt.Sprintf("[%d more lines in file. Use offset=%d to continue.]", remaining, nextOffset)
	default:
		nextOffset = lastLine + 1
		footer = fmt.Sprintf("[Showing lines %d-%d of %d. Use offset=%d to continue.]",
			firstLine, lastLine, total, nextOffset)
	}

	return map[string]any{
		"path":        res.DisplayPath,
		"size":        info.Size(),
		"total_lines": total,
		"offset":      firstLine,
		"next_offset": nextOffset,
		"content":     strings.Join(window, "") + "\n" + footer,
	}, nil
}

// truncateLongLine 截断超长的行，行尾补标记（DSH 的行内截断标记风格：
// "... (line truncated to N chars)"）。
//
// 为什么要它：minified JS / 压缩过的 JSON 这类文件**一整行**能有几百 KB，
// 只靠总量上限的话，一次读取会把整个预算吃光，模型什么也读不到。
func truncateLongLine(line string) string {
	if len([]rune(line)) <= maxLineChars {
		return line
	}
	runes := []rune(line)
	return string(runes[:maxLineChars]) + fmt.Sprintf("... (line truncated to %d chars)", maxLineChars)
}

// WriteFile 写入或追加文件内容。
//
// 路径先经 filesystem 能力以 OpWrite 解析（Lexical + Realpath 双层，防 symlink
// 逃逸与经链接创建），越界返回 *filesystem.PathDeniedError。
type WriteFile struct {
	FS filesystem.FileSystem
	// Gate 是写前观察门（可选，nil = 不启用）：write 前校验"读过才许覆盖、
	// 读后变了要重读"，写后记录新版本（S5h）。
	Gate *ObserveGate
}

// NewWriteFile 构造写文件工具。
func NewWriteFile(fsys filesystem.FileSystem) *WriteFile { return &WriteFile{FS: fsys} }

// NewWriteFileObserved 构造带观察门的写文件工具（S5h）。
func NewWriteFileObserved(fsys filesystem.FileSystem, gate *ObserveGate) *WriteFile {
	return &WriteFile{FS: fsys, Gate: gate}
}

type writeFileParams struct {
	Path    string `json:"path" mos:"desc=文件路径（相对路径以工作目录为锚点）"`
	Content string `json:"content" mos:"desc=要写入的完整内容"`
}

func (WriteFile) Name() string { return "write_file" }

func (WriteFile) Description() string {
	// 结果里带 created / replaced_bytes：模型据此知道自己是新建还是覆盖了已有
	// 内容（DSH 的 write 区分 Created/Updated，PI 的 edit 报告改了几处）——
	// 没有这个信息，模型只能读回来确认，反复重写就容易空转。
	// 刻意**不做追加写**（DSH 也没有）：append 让「这次到底改了什么」变得含糊，
	// 而追加场景用 edit_file 精确追加更可控。
	return "写入文本到文件（整体覆盖，自动创建父目录；受工作目录沙箱限制）。" +
		"只改局部内容请优先用 edit_file。覆盖已有文件前建议先 read_file 确认——" +
		"结果里的 created 表示本次是否新建，replaced_bytes 表示被覆盖丢弃的原有字节数。"
}

func (WriteFile) Parameters() any { return &writeFileParams{} }

// Guideline 实现 tool.Guideline：写前观察门（ObserveGate）开启时，把前置条件
// 讲在系统提示词里，而不是让模型撞了拒绝才学会——每次拒绝都是一轮浪费的往返。
// 门未开启时返回空串（不贡献）：没强制不了的事就别承诺。
func (w *WriteFile) Guideline() string {
	if w.Gate == nil {
		return ""
	}
	return "覆盖已有文件前必须先 read_file：本会话没读过的文件不允许覆盖（写前观察门会拒绝），" +
		"读过之后若文件又被改过，需重读后再写。新建文件不需要先读。"
}

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
	// 写前观察（S5h）：不许覆盖没读过的文件；读后被改过/删过要先重读。
	if err := w.Gate.CheckWrite(target, res.DisplayPath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	// 写入前的状态：新建还是覆盖？覆盖掉了多少原有内容？
	// 模型需要这个才知道自己有没有冲掉已有内容（否则只能读回来确认，容易空转）。
	// created 对应 DSH 的 Created/Updated 措辞。
	created := true
	var oldSize int64
	if info, err := os.Stat(target); err == nil {
		created = false
		oldSize = info.Size()
	}
	// 原子写：临时文件 + rename（DSH 的 writeFileAtomic 同样做法）——
	// 写一半失败不会把原文件截成半个。
	if err := atomicWrite(target, []byte(p.Content), 0o644); err != nil {
		return nil, err
	}
	// 写成功 = 新的权威状态（S5h）：记录新版本，后续 edit 以此为 CAS 基准。
	if v, present, err := fileVersion(target); err == nil && present {
		w.Gate.Record(target, v, true)
	}
	return map[string]any{
		"path":           res.DisplayPath,
		"bytes":          len(p.Content),
		"created":        created,
		"replaced_bytes": oldSize,
	}, nil
}

var _ tool.Tool = (*WriteFile)(nil)

// FileTools 返回受文件能力约束的工具集（读 + 写 + 精确编辑）。
//
// gate 为写前观察门（S5h）：非 nil 时启用「不许覆盖没读过的文件、读后被改
// 要先重读」；nil 时不启用（旧行为：无条件变更）。**能力由宿主决定**——
// 与 shell 的 -allow-shell 同一哲学。
//
// 宿主没装配文件能力时可以不调用本函数——不给模型这个能力，比给它一个
// 必然失败的工具更干净。
func FileTools(fsys filesystem.FileSystem, gate *ObserveGate) []tool.Tool {
	return []tool.Tool{NewReadFileObserved(fsys, gate), NewWriteFileObserved(fsys, gate), NewEditObserved(fsys, gate)}
}

// FileToolsIn 返回一套**文件工具**（read_file / write_file / edit_file），
// 装好两件事：
//
//   - 文件 jail：根目录 = root，越界路径被拒（Lexical + Realpath 双校验，
//     symlink 逃逸也挡住）；
//   - 写前观察门：没读过的文件不许覆盖，读过之后被改过要先重读（S5h）。
//
// root 为空 → 用当前目录。要定制的宿主（共享 fsys、自定义观察门、读写根
// 不同）用 FileTools(fsys, gate) 自己拼。
//
// 便利入口：宿主说"我要一套以 root 为界的文件工具"，框架管 jail + 观察门。
func FileToolsIn(root string) ([]tool.Tool, error) {
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("builtin: 取工作目录: %w", err)
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("builtin: 解析文件工具根 %q: %w", root, err)
	}
	fsys := filesystem.NewSandboxed(&filesystem.FileAccessPolicy{
		ReadRoots:  []string{abs},
		WriteRoots: []string{abs},
	})
	return FileTools(fsys, NewObserveGate()), nil
}
