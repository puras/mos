package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// ResultTruncator 对"给模型看的工具结果文本"做上下文瘦身（M2.3）。
// 截断只作用于模型可见的文本，工具原始返回值（含审计）不受影响。
type ResultTruncator interface {
	// Truncate 返回替换后的模型可见文本。发生截断时返回 truncated=true，
	// 返回值应携带逃生通道提示（完整输出的落盘路径与按需回读方式）。
	Truncate(toolName, content string) (result string, truncated bool)
}

// TruncatorConfig 是默认截断器的配置。
type TruncatorConfig struct {
	// MaxBytes 单次工具结果最大字节数（0 表示不限制字节）。
	MaxBytes int
	// MaxLines 单次工具结果最大行数（0 表示不限制行数）。
	MaxLines int
	// Dir 逃生通道落盘目录（空 = os.TempDir()）。
	// 截断时完整结果写入该目录的临时文件，提示模型用 read_file 按偏移回读。
	Dir string
}

// 默认上限：参考 Claude Code 单工具 50K 字符 / OpenClaw 预算占比 / Pi 50KB。
// shell 沙箱默认输出上限同为 64KB，双层防护互不冲突。
const (
	DefaultTruncateMaxBytes = 64 * 1024
	DefaultTruncateMaxLines = 2000
)

// headTools 的完整输出从头读更有价值（文件结构/概要在头部），截断时保留头部。
var headTools = map[string]bool{
	"read_file":    true,
	"web_fetch":    true,
	"web_search":   true,
	"grep":         true,
	"find_files":   true,
	"current_time": true,
}

// tailTools 的关键信息在尾部（命令失败报错/堆栈在最后），截断时保留尾部。
var tailTools = map[string]bool{
	"shell": true,
}

// NewDefaultTruncator 创建默认截断器：行数/字节双重限制 + 按工具类型的
// 智能截断（head 类保留头部 / tail 类保留尾部 / 其余保留 head+tail）
// + 逃生通道（完整结果落盘，模型可经 read_file 按偏移回读）。
// cfg 为零值时使用默认上限（64KB / 2000 行）。
func NewDefaultTruncator(cfg TruncatorConfig) ResultTruncator {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultTruncateMaxBytes
	}
	if cfg.MaxLines <= 0 {
		cfg.MaxLines = DefaultTruncateMaxLines
	}
	if cfg.Dir == "" {
		cfg.Dir = os.TempDir()
	}
	return &defaultTruncator{cfg: cfg}
}

type defaultTruncator struct {
	cfg TruncatorConfig
	seq atomic.Uint64
}

func (d *defaultTruncator) Truncate(toolName, content string) (string, bool) {
	// 双重限制：行数与字节，谁先超限都触发截断
	overBytes := d.cfg.MaxBytes > 0 && len(content) > d.cfg.MaxBytes
	lines := 1 + strings.Count(content, "\n")
	overLines := d.cfg.MaxLines > 0 && lines > d.cfg.MaxLines
	if !overBytes && !overLines {
		return content, false
	}

	keep := d.cfg.MaxLines
	if keep <= 0 || lines <= keep {
		keep = lines
	}
	var out string
	switch {
	case headTools[toolName]:
		out = headTruncate(content, keep)
	case tailTools[toolName]:
		out = tailTruncate(content, keep)
	default:
		out = bothTruncate(content, keep)
	}
	if d.cfg.MaxBytes > 0 && len(out) > d.cfg.MaxBytes {
		out = byteTruncate(out, d.cfg.MaxBytes)
	}

	// 逃生通道：完整结果落盘，模型需要细节时按偏移回读
	spillPath := d.spill(toolName, content)
	note := buildTruncateNote(toolName, len(content), lines, spillPath)
	return out + note, true
}

// spill 将完整工具结果写入临时文件（M2.3 逃生通道），返回路径。
// 写入失败时降级为"无路径"提示，截断本体不受影响。
func (d *defaultTruncator) spill(toolName, content string) string {
	dir := filepath.Join(d.cfg.Dir, "mos-trunc")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	name := filepath.Join(dir, "mos-"+toolName+"_"+itoa(int(d.seq.Add(1)))+".txt")
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		return ""
	}
	return name
}

func buildTruncateNote(toolName string, totalBytes, totalLines int, path string) string {
	var b strings.Builder
	b.WriteString("\n\n[工具输出已截断：完整 ")
	b.WriteString(itoa(totalBytes))
	b.WriteString(" 字节 / ")
	b.WriteString(itoa(totalLines))
	b.WriteString(" 行。")
	if path != "" {
		b.WriteString("完整输出已保存至 ")
		b.WriteString(path)
		b.WriteString("，需要完整内容请用 read_file(path=\"")
		b.WriteString(path)
		b.WriteString("\") 按偏移回读。]")
	} else {
		b.WriteString("需要更多内容请改用其他工具按需获取。]")
	}
	return b.String()
}

// headTruncate 保留开头 keep 行（head 类工具：read_file / web_fetch / grep 等）。
func headTruncate(s string, keep int) string {
	idx := nthNewline(s, keep)
	if idx < 0 {
		return s
	}
	return s[:idx] + "\n... [中间省略 " + itoa(1+strings.Count(s[idx:], "\n")) + " 行] ...\n"
}

// tailTruncate 保留结尾 keep 行（tail 类工具：shell 等，报错在尾部）。
func tailTruncate(s string, keep int) string {
	idx := nthNewlineFromEnd(s, keep)
	if idx < 0 {
		return s
	}
	return "... [前面省略 " + itoa(1+strings.Count(s[:idx+1], "\n")) + " 行] ...\n" + s[idx+1:]
}

// bothTruncate 保留 head+tail 各 keep/2 行（通用策略，中间省略）。
func bothTruncate(s string, keep int) string {
	half := keep / 2
	if half < 1 {
		half = 1
	}
	start := nthNewline(s, half)
	if start < 0 {
		return s
	}
	head := s[:start] + "\n"
	rest := s[start:]
	end := nthNewlineFromEnd(rest, half)
	if end < 0 {
		return s
	}
	omitted := 1 + strings.Count(rest[:end+1], "\n")
	tail := rest[end+1:]
	return head + "... [中间省略 " + itoa(omitted) + " 行] ...\n" + tail
}

// byteTruncate 按字节截断（UTF-8 安全：不切断多字节字符），保留开头。
func byteTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	if n == 0 {
		return ""
	}
	return s[:n] + "\n... [输出过长已截断] ...\n"
}

// nthNewline 返回第 n 个 '\n' 的索引（1-based）；不存在返回 -1。
func nthNewline(s string, n int) int {
	idx := -1
	for i := 0; i < n; i++ {
		j := strings.IndexByte(s[idx+1:], '\n')
		if j < 0 {
			return -1
		}
		idx += j + 1
	}
	return idx
}

// nthNewlineFromEnd 从尾部数第 n 个 '\n' 的索引；不存在返回 -1。
func nthNewlineFromEnd(s string, n int) int {
	idx := len(s)
	for i := 0; i < n; i++ {
		j := strings.LastIndexByte(s[:idx], '\n')
		if j < 0 {
			return -1
		}
		idx = j
	}
	return idx
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
