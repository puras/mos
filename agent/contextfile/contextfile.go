// Package contextfile 发现并合并「上下文文件」（AGENTS.md / CLAUDE.md 等）。
//
// 站位：**机制在框架，策略在宿主**。本包只做三件事——发现、合并、渲染；
// 「要不要读」「读哪些目录」「超预算怎么办」都是宿主的决定：宿主知道自己的
// 部署形态（项目根在哪、全局约定放哪、上下文预算多少）。
//
// 全局目录由宿主显式传入（Options.GlobalDirs）：框架不猜路径，也不隐式读用户
// HOME——库替宿主读目录是惊喜，不是能力。但 MoS 自己的全局目录名是**定的**：
// `~/.mos`（DefaultGlobalDir）——与 PI 的 `~/.pi/agent` 无关，各家各的。
//
// **框架不碰文件系统**（ARCHITECTURE.md §4 治理规则 5）：读文件的动作以
// Reader 接口注入，宿主选实现。要 jail 语义（只读宿主声明的根内）就注入
// 自己的 Reader；不注入时用 OSReader（直接读本地文件）。
//
// 发现规则（对齐 PI 的 packages/coding-agent/src/core/resource-loader.ts）：
//
//   - 候选名按序取**第一个命中**的文件：AGENTS.override.md > AGENTS.md >
//     AGENTS.MD > CLAUDE.md > CLAUDE.MD。同一目录里 override 因此**替换**同目录的
//     AGENTS.md / CLAUDE.md，但**不影响**其它目录的上下文文件（PI 同款语义）。
//   - 扫描顺序：全局目录 → 各级祖先目录（由外向内）→ 工作目录。越靠近工作目录的
//     约定排在**后面**（更具体的在后生效）。
//   - 同一文件只加载一次（按真实路径去重：软链到同一份文件算一份）。
//     PI 用「worktree 遮蔽」专门处理 git worktree 的重复加载，这里用真实路径
//     去重覆盖同一类问题（少一个 git 依赖，语义更宽）。
//
// 合并后的正文按 PI 同款包装成 <project_instructions path="..."> 块——模型据此
// 知道每条约定来自哪个文件；包装文本与正文都计入字节预算。
//
// 预算语义：超出上限时**裁当前文件**（行边界优先，Rune 边界兜底），并如实
// 标注保留了多少字节；后续更具体的文件不再加载（预算用完就是用完了），
// 降级说明进 Result.Warnings——不静默丢内容。
package contextfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Reader 是上下文文件的读取能力（框架不碰文件系统，I/O 由宿主注入）。
//
// 三个动作够用：判「是不是普通文件」、读内容、解析真实路径（同一份文件去重）。
type Reader interface {
	// Stat 报告路径的文件信息。
	Stat(path string) (os.FileInfo, error)
	// ReadFile 读文件内容。
	ReadFile(path string) ([]byte, error)
	// Canonical 返回路径的规范形式（软链解析）：同一份文件只算一份。
	Canonical(path string) string
}

// OSReader 是直接读本地文件的 Reader（宿主不注入时的默认实现）。
//
// 需要 jail 语义（只许读宿主声明的根内）就注入自己的 Reader——框架不替宿主
// 决定能读哪儿。
type OSReader struct{}

// Stat 实现 Reader。
func (OSReader) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }

// ReadFile 实现 Reader。
func (OSReader) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// Canonical 实现 Reader：软链解析失败时退回清理过的路径（去重退一步，不影响加载）。
func (OSReader) Canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// DefaultCandidates 是候选上下文文件名（按优先级）。
//
// 第一个命中即采用：AGENTS.override.md 天然地「替换」同目录的
// AGENTS.md / CLAUDE.md——override 的语义由候选顺序表达，不另设分支。
var DefaultCandidates = []string{
	"AGENTS.override.md",
	"AGENTS.md",
	"AGENTS.MD",
	"CLAUDE.md",
	"CLAUDE.MD",
}

// DefaultMaxBytes 是合并正文的默认字节上限（约 8K token，与工具结果截断同量级）。
//
// 上下文文件是**静态**内容：它进系统提示词，不会被工具结果截断那道网兜住
// （截断只作用于工具结果）。所以这里必须自带上限——否则一个写得多的
// AGENTS.md 能把上下文预算吃掉一大块，而且吃掉的部分还是常驻的。
const DefaultMaxBytes = 32 * 1024

// DefaultGlobalDirName 是 MoS 全局目录名（相对用户 HOME）。
//
// `~/.mos` 是 **MoS 自己的**目录（全局上下文约定等）；与产品层的 `~/.mos-web`
// （如 mos-web 的会话状态）不是一回事，也与 PI 的 `~/.pi/agent` 无关。
const DefaultGlobalDirName = ".mos"

// DefaultGlobalDir 返回 MoS 的全局上下文目录（~/.mos）。
//
// 取不到 HOME 时返回空串（宿主据此跳过，不报错——没有 HOME 不是故障）。
func DefaultGlobalDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, DefaultGlobalDirName)
}

// ExpandHome 展开路径开头的 "~"（宿主常直接写 "~/.mos"）。
// 不以 "~" 开头的路径原样返回。
func ExpandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// 包装格式（PI 同款）：模型据此知道每条约定来自哪个文件。
const (
	blockOpenFormat  = "<project_instructions path=%q>\n"
	blockClose       = "</project_instructions>"
	sectionHeader    = "项目约定与指引（来自以下上下文文件）："
	sectionSeparator = "\n\n"
	// truncNoteReserve 是截断说明预留的字节数（说明文本本身也进提示词）。
	truncNoteReserve = 128
)

// Options 是发现参数。
//
// 零值 = 保守默认：只看工作目录本身、32KB 预算、不带全局目录。
// 想向上递归（拿到仓库根的 AGENTS.md）或读全局约定，宿主显式开口。
type Options struct {
	// Candidates 是候选文件名（按优先级）。空 = DefaultCandidates。
	Candidates []string
	// GlobalDirs 是全局目录（用户级约定）。按给出的顺序加载：先给的更一般，
	// 后给的更具体（后加载的排在合并结果更靠后）。
	// 路径可以写成 "~/.mos"（自动展开，见 ExpandHome）。
	GlobalDirs []string
	// MaxBytes 是合并正文的字节上限。0 = DefaultMaxBytes；负数 = 不限。
	MaxBytes int
	// MaxDepth 是向上递归的层数（工作目录本身是第 0 层）。
	// 0 = 只看工作目录；正数 = 工作目录 + 最多 N 层祖先；负数 = 不限（走到根）。
	MaxDepth int
	// Reader 是读文件的实现。nil = OSReader（直接读本地文件）。
	// 要 jail 语义就注入宿主自己的实现（框架不替宿主决定能读哪儿）。
	Reader Reader
}

// Loader 是上下文文件发现器。构造后只读，可并发使用。
type Loader struct {
	candidates []string
	globalDirs []string
	maxBytes   int
	maxDepth   int
	reader     Reader
}

// NewLoader 按参数创建发现器。
func NewLoader(opts Options) *Loader {
	candidates := opts.Candidates
	if len(candidates) == 0 {
		candidates = DefaultCandidates
	}
	maxBytes := opts.MaxBytes
	switch {
	case maxBytes == 0:
		maxBytes = DefaultMaxBytes
	case maxBytes < 0:
		maxBytes = 0 // 不限
	}
	reader := opts.Reader
	if reader == nil {
		reader = OSReader{}
	}
	globalDirs := make([]string, 0, len(opts.GlobalDirs))
	for _, d := range opts.GlobalDirs {
		if d == "" {
			continue
		}
		globalDirs = append(globalDirs, ExpandHome(d))
	}
	return &Loader{
		candidates: append([]string(nil), candidates...),
		globalDirs: globalDirs,
		maxBytes:   maxBytes,
		maxDepth:   opts.MaxDepth,
		reader:     reader,
	}
}

// Load 一次性发现并合并（Options 的便捷入口）。
func Load(dir string, opts Options) (Result, error) {
	return NewLoader(opts).Load(dir)
}

// File 是一个命中的上下文文件。
type File struct {
	// Path 是绝对路径。
	Path string
	// Content 是正文（已去 BOM；超预算时被裁过）。
	Content string
	// Bytes 是正文的字节数（去 BOM 后；裁过之后是裁后的长度）。
	Bytes int
	// Truncated 标记该文件是否因字节预算被裁过。
	Truncated bool
}

// Result 是一次发现的产物。
type Result struct {
	// Files 是加载到的文件（合并顺序：全局 → 由外向内的祖先 → 工作目录）。
	Files []File
	// Text 是渲染后的正文（可直接作为系统提示词的一节）。
	Text string
	// Warnings 是降级说明（读不动的文件、被预算挡掉的文件……）。
	Warnings []string
	// Truncated 标记是否有文件因字节预算被裁过。
	Truncated bool
}

// Load 发现并合并 dir 及其祖先目录（以及全局目录）里的上下文文件。
//
// 工作目录本身不存在或不是目录时返回错误（调用方的配置错了，不是可降级的故障）；
// 单个文件读不动、被预算挡住等按「降级」处理：如实记进 Warnings，继续往下走。
func (l *Loader) Load(dir string) (Result, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, fmt.Errorf("contextfile: 取绝对路径 %q: %w", dir, err)
	}
	info, err := l.reader.Stat(abs)
	if err != nil {
		return Result{}, fmt.Errorf("contextfile: 读取工作目录 %q: %w", abs, err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("contextfile: 工作目录 %q 不是目录", abs)
	}

	var res Result
	seen := make(map[string]bool)
	for _, d := range l.scanDirs(abs) {
		f, warnings := l.loadFromDir(d)
		res.Warnings = append(res.Warnings, warnings...)
		if f == nil {
			continue
		}
		key := l.reader.Canonical(f.Path)
		if seen[key] {
			// 同一份文件经软链被扫到两次：算一份（PI 的 worktree 遮蔽同款问题）。
			continue
		}
		seen[key] = true
		res.Files = append(res.Files, *f)
	}
	l.render(&res)
	return res, nil
}

// scanDirs 返回要扫描的目录（合并顺序）：全局目录 → 各级祖先（由外向内）→ 工作目录。
func (l *Loader) scanDirs(workdir string) []string {
	dirs := make([]string, 0, len(l.globalDirs)+4)
	dirs = append(dirs, l.globalDirs...)

	// 祖先链先按「工作目录 → 父目录」收集，最后**倒序**放进结果：越外层的约定
	// 越一般，排在前面；越靠近工作目录的约定越具体，排在后面（PI 同款顺序）。
	ancestors := []string{workdir}
	cur := workdir
	for depth := 0; ; depth++ {
		if l.maxDepth >= 0 && depth >= l.maxDepth {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
		ancestors = append(ancestors, cur)
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		dirs = append(dirs, ancestors[i])
	}
	return dirs
}

// loadFromDir 在一个目录里取第一个命中的候选文件。
//
// 读不动就记警告并**继续试下一个候选**（PI 同款：文件坏了不等于这个目录没有约定）。
func (l *Loader) loadFromDir(dir string) (*File, []string) {
	var warnings []string
	for _, name := range l.candidates {
		path := filepath.Join(dir, name)
		info, err := l.reader.Stat(path)
		if err != nil {
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("上下文候选 %s 读取失败：%v（已跳过）", path, err))
			}
			continue
		}
		if !info.Mode().IsRegular() {
			warnings = append(warnings, fmt.Sprintf("上下文候选 %s 不是普通文件（已跳过）", path))
			continue
		}
		raw, err := l.reader.ReadFile(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("上下文文件 %s 读取失败：%v（已跳过）", path, err))
			continue
		}
		content := stripBOM(string(raw))
		return &File{Path: path, Content: content, Bytes: len(content)}, warnings
	}
	return nil, warnings
}

// render 把文件渲染成合并正文，并按预算裁剪。
//
// 只进正文的文件才留在 Result.Files 里（发现但装不下的不算「加载到了」，
// 否则 Files 与 Text 会各说各话）。
func (l *Loader) render(res *Result) {
	if len(res.Files) == 0 {
		return
	}
	included := make([]File, 0, len(res.Files))
	blocks := make([]string, 0, len(res.Files))
	used := len(sectionHeader) + len(sectionSeparator)
	skipped := 0
	truncatedAny := false

	for i := range res.Files {
		f := res.Files[i]
		open := fmt.Sprintf(blockOpenFormat, f.Path)
		// 预留截断说明的位置（说明本身也进提示词，不能让它把预算顶穿）。
		overhead := len(open) + len(blockClose) + truncNoteReserve

		content := f.Content
		total := f.Bytes
		cut := false
		if l.maxBytes > 0 {
			remaining := l.maxBytes - used - overhead - len(sectionSeparator)
			if remaining <= 0 {
				// 预算用完了：更具体的文件不再加载（如实记警告，不静默丢）。
				skipped++
				continue
			}
			content, cut = cutToBudget(f.Content, remaining)
		}

		note := ""
		if cut {
			note = fmt.Sprintf("\n[上下文文件超出字节预算，已截断：保留前 %d 字节 / 共 %d 字节]\n",
				len(content), total)
			f.Content = content
			f.Bytes = len(content)
			f.Truncated = true
			truncatedAny = true
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"上下文文件 %s 超出字节预算，已截断（保留前 %d 字节 / 共 %d 字节）",
				f.Path, len(content), total))
		}
		blocks = append(blocks, open+content+note+blockClose)
		included = append(included, f)
		used += len(open) + len(content) + len(note) + len(blockClose) + len(sectionSeparator)
	}

	res.Files = included
	res.Truncated = truncatedAny
	if skipped > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"后续 %d 个上下文文件因字节预算不足未加载", skipped))
	}
	if len(included) == 0 {
		// 一个都没装下：不留空标题（空正文 = 没有上下文文件）。
		res.Text = ""
		return
	}
	res.Text = sectionHeader + sectionSeparator + strings.Join(blocks, sectionSeparator)
}

// cutToBudget 把正文裁到 limit 字节内：优先切在行边界，切不到就退回 Rune 边界
// （不切碎多字节字符）。返回裁后的正文与是否裁过。
func cutToBudget(s string, limit int) (string, bool) {
	if limit <= 0 {
		return "", len(s) > 0
	}
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	// 回退到 Rune 边界：s[cut] 是切点后的第一个字节，是续接字节就说明切碎了一个字符。
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	// 行边界优先——但别为了一行尾巴丢掉大半预算（超长行按 Rune 边界切）。
	if idx := strings.LastIndexByte(s[:cut], '\n'); idx > 0 && cut-idx <= limit/2 {
		return s[:idx+1], true
	}
	return s[:cut], true
}

// stripBOM 去 UTF-8 BOM（PI 同款）：带 BOM 的正文进提示词会在第一行留下乱码字符。
func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}
