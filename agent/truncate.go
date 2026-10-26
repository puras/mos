package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/puras/mos/agent/hook"
)

// ResultTruncator 对"给模型看的工具结果文本"做瘦身（截断只作用于模型可见
// 的文本；会话日志里落的是截断后的回填——日志记的就是模型实际看到的东西）。
type ResultTruncator interface {
	// Truncate 返回替换后的文本与是否发生了截断。截断时的返回值应自带
	// 说明（少了多少、怎么补读），模型据此决定要不要取全文。
	Truncate(toolName, content string) (result string, truncated bool)
}

// TruncatorConfig 是默认截断器的配置（零值字段取对应默认值）。
type TruncatorConfig struct {
	// MaxBytes 单次结果最大字节数（<=0 取 DefaultTruncateMaxBytes）。
	MaxBytes int
	// MaxLines 单次结果最大行数（<=0 取 DefaultTruncateMaxLines）。
	MaxLines int
	// Dir 是逃生通道的落盘目录。**空 = 不落盘**：提示里只说截断了多少，
	// 不给路径。给目录时截断即写全文，提示带路径——但宿主必须把该目录
	// 放进文件 jail 的可读根，模型才真的读得到（read_file 走 jail）；
	// 放在 jail 外的路径是一个模型永远兑现不了的承诺。
	Dir string
}

// 默认上限：单工具 64KB / 2000 行（对位 PI 的 50KB 与 DSH 的输出上限；
// sandbox 的进程输出上限是另一层——这里管所有工具的回填文本）。
const (
	DefaultTruncateMaxBytes = 64 * 1024
	DefaultTruncateMaxLines = 2000
)

// 按工具定向的保留策略判定表：头部价值高（结构/概要在前）的保留头部；
// 尾部价值高（报错/堆栈在后）的保留尾部；其余两头各留一半。
// （表随内置工具集生长；宿主自定义工具默认走两头各半。）
var (
	headTools = map[string]bool{
		"read_file":    true, // 文件开头是结构；shell 的报错却在尾部
		"current_time": true,
	}
	tailTools = map[string]bool{
		"shell": true, // 编译/测试的报错与摘要几乎总在输出末尾
	}
)

// NewDefaultTruncator 创建默认截断器：行数/字节双限制 + 按工具定向的保留
// 策略（头部/尾部/两头）+ 可选落盘逃生通道。
func NewDefaultTruncator(cfg TruncatorConfig) ResultTruncator {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultTruncateMaxBytes
	}
	if cfg.MaxLines <= 0 {
		cfg.MaxLines = DefaultTruncateMaxLines
	}
	return &defaultTruncator{cfg: cfg}
}

type defaultTruncator struct {
	cfg TruncatorConfig
	seq atomic.Uint64
}

// Truncate 实现 ResultTruncator。
func (d *defaultTruncator) Truncate(toolName, content string) (string, bool) {
	overBytes := len(content) > d.cfg.MaxBytes
	overLines := 1+strings.Count(content, "\n") > d.cfg.MaxLines
	if !overBytes && !overLines {
		return content, false
	}

	out := clipLines(toolName, content, d.cfg.MaxLines)
	if len(out) > d.cfg.MaxBytes {
		out = clipBytes(out, d.cfg.MaxBytes)
	}

	// 逃生通道：全文落盘，模型按需回读；写不了就诚实地只报截断。
	out += "\n" + d.note(toolName, content)
	return out, true
}

// note 生成截断说明：少给了什么、全文在哪（或为什么没落盘）。
func (d *defaultTruncator) note(toolName, content string) string {
	var b strings.Builder
	b.WriteString("[工具输出已截断：完整 ")
	b.WriteString(strconv.Itoa(len(content)))
	b.WriteString(" 字节 / ")
	b.WriteString(strconv.Itoa(1 + strings.Count(content, "\n")))
	b.WriteString(" 行。")
	if d.cfg.Dir != "" {
		if path := d.spill(toolName, content); path != "" {
			b.WriteString("全文已保存至 ")
			b.WriteString(path)
			b.WriteString("，需要细节时用 read_file(path=\"")
			b.WriteString(path)
			b.WriteString("\") 分段读取。]")
			return b.String()
		}
		b.WriteString("全文落盘失败，")
	}
	b.WriteString("请改用分次读取（如 read_file 的 offset/limit）或更精确的查询获取细节。]")
	return b.String()
}

// spill 把完整结果写入落盘目录，返回文件路径；失败返回空串（截断本体不受影响）。
func (d *defaultTruncator) spill(toolName, content string) string {
	if err := os.MkdirAll(d.cfg.Dir, 0o700); err != nil {
		return ""
	}
	name := filepath.Join(d.cfg.Dir,
		"trunc-"+toolName+"-"+strconv.FormatUint(d.seq.Add(1), 10)+".txt")
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		return ""
	}
	return name
}

// clipLines 按工具的保留策略留行数预算内的内容：头部 / 尾部 / 两头各半。
func clipLines(toolName, s string, keep int) string {
	lines := 1 + strings.Count(s, "\n")
	if lines <= keep {
		return s
	}
	switch {
	case headTools[toolName]:
		return keepHead(s, keep)
	case tailTools[toolName]:
		return keepTail(s, keep)
	default:
		return keepBoth(s, keep)
	}
}

// keepHead 保留开头 keep 行，中段以省略标记代替。
func keepHead(s string, keep int) string {
	idx := nthNewline(s, keep)
	if idx < 0 {
		return s
	}
	return s[:idx] + "\n... [中段省略 " + strconv.Itoa(1+strings.Count(s[idx:], "\n")) + " 行] ..."
}

// keepTail 保留结尾 keep 行（报错在尾部的工具）。
func keepTail(s string, keep int) string {
	idx := nthNewlineFromEnd(s, keep)
	if idx < 0 {
		return s
	}
	return "... [前面省略 " + strconv.Itoa(1+strings.Count(s[:idx+1], "\n")) + " 行] ...\n" + s[idx+1:]
}

// keepBoth 两头各留一半（概要与结论都在，中间的体量让位）。
func keepBoth(s string, keep int) string {
	half := keep / 2
	if half < 1 {
		half = 1
	}
	headIdx := nthNewline(s, half)
	if headIdx < 0 {
		return s
	}
	rest := s[headIdx:]
	tailIdx := nthNewlineFromEnd(rest, half)
	if tailIdx <= 0 {
		return s
	}
	omitted := 1 + strings.Count(rest[:tailIdx+1], "\n")
	return s[:headIdx] + "\n... [中段省略 " + strconv.Itoa(omitted) + " 行] ...\n" + rest[tailIdx+1:]
}

// clipBytes 按字节截断，UTF-8 边界安全（多字节字符不切半）。
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	if n == 0 {
		return "... [输出过长已截断] ..."
	}
	return s[:n] + "\n... [输出过长已截断] ..."
}

// nthNewline 返回第 n 个 '\n' 的下标（n 从 1 起）；不存在返回 -1。
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

// nthNewlineFromEnd 返回从尾部数第 n 个 '\n' 的下标；不存在返回 -1。
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

// TruncateHook 把截断器接进 tool/post-execute 环节：宿主装配一行——
//
//	hooks.On(hook.HookToolPostExecute, agent.TruncateHook(trunc))
//
// 钩子只改写回填文本（ResultText），不碰载荷（载荷冻结）；错误结果同样
// 可能巨大（崩溃堆栈），一并截断。截断器为 nil 时透传（零开销降级）。
func TruncateHook(t ResultTruncator) hook.Hook {
	if t == nil {
		return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
			return next(ctx, ev)
		}
	}
	return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
		out, err := next(ctx, ev)
		if err != nil || out == nil {
			return out, err
		}
		if after, truncated := t.Truncate(ev.ToolCall.Name, out.ResultText); truncated {
			out.ResultText = after
		}
		return out, nil
	}
}
