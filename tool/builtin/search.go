package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/tool"
)

// 搜索三件套（grep / glob / ls）：纯只读、走文件 jail、结果有上限。
//
// 为什么要有（对照 PI/DSH）：模型要"找东西"只有两条路——挨个文件 read，
// 或者退回去用 shell 跑 rg。后者把搜索塞进命令字符串（参数注入面 + 噪声大
// + 受沙箱策略限制），前者是线性试错。搜索是**一等能力**：只读、可并发、
// 结果可控。
//
// 与 shell 的分工：grep/glob/ls 是**结构化**的（参数化、可复现、无注入面），
// shell 是**通用**的（编译、测试、管道）。能用搜索解决的，别退化成 shell。
//
// jail 安全性（重要）：搜索的起点经 filesystem 能力解析（Lexical + Realpath
// 双校验），拿到的是**实体路径**；随后只在实体路径下做词法遍历，符号链接
// 不是常规文件、天然被跳过——根内一个指向 /etc/passwd 的链接不会被读到，
// 也不会被跟随进目录。逐个文件再走一遍 Resolve 是多余的：遍历路径恒在
// 已校验的实体根内。

const (
	// defaultGrepLimit 是 grep 默认返回的匹配条数上限。
	defaultGrepLimit = 100
	// maxGrepLimit 是 grep 允许的硬上限（模型给的 limit 会被夹到这里）。
	maxGrepLimit = 1000
	// defaultGlobLimit 是 glob 默认返回的匹配文件数上限（PI 的 find：1000）。
	defaultGlobLimit = 1000
	// maxGlobLimit 是 glob 允许的硬上限。
	maxGlobLimit = 10000
	// defaultLsLimit 是 ls 默认返回的条目数上限（PI 的 ls：500）。
	defaultLsLimit = 500
	// maxLsLimit 是 ls 允许的硬上限。
	maxLsLimit = 5000
	// maxSearchFiles 是单次搜索最多扫描的文件数（预算用尽即停，并如实标注）。
	maxSearchFiles = 20000
	// maxSearchFileBytes 是单文件最多扫描的字节数（只扫前缀；超出部分不搜）。
	maxSearchFileBytes = 1 << 20
	// binarySniffBytes 是二进制嗅探的取样字节数（含 NUL 即判为二进制）。
	binarySniffBytes = 8192
)

// skippedDirs 是搜索时**跳过**的目录名（遍历到即 SkipDir）。
//
// 只跳 .git：它是对象库，几 MB 的散列文件对模型毫无用处，却能让一次
// grep 扫上几万文件。其余目录（node_modules 等）不跳——宿主可以用更窄的
// path 参数收窄范围，框架不替宿主猜什么该被看见。
var skippedDirs = map[string]bool{".git": true}

// Grep 在文件内容里搜索正则，返回匹配行（ripgrep 风格 path:行号:内容）。
//
// 只读工具：输出即环境回答（可进 TaskState，审计门白名单内）。
type Grep struct {
	FS filesystem.FileSystem
	// Exclude 是**宿主策略**的排除谓词：返回 true 的文件不参与搜索
	//（不读、不报命中）。
	//
	// 为什么需要它：宿主可以在参数级规则里拒绝 read 敏感文件（.env / 私钥），
	// 但 grep 是**按内容搜**——路径参数通常是个目录，逐文件判定做不了。
	// 没有这个挂点，“不许把凭据读进模型上下文”的保证就被 grep 绕过了。
	//
	// 策略归宿主（什么算敏感），框架只提供挂点。nil = 不排除。
	Exclude func(path string) bool
}

// NewGrep 构造内容搜索工具。
func NewGrep(fsys filesystem.FileSystem) *Grep { return &Grep{FS: fsys} }

func (Grep) Name() string { return "grep" }

func (Grep) Description() string {
	return "按正则搜索文件内容，返回匹配行（格式 path:行号:内容；受工作目录沙箱限制）。" +
		"参数：pattern=正则表达式（RE2 语法，必填）；path=搜索目录或文件（默认工作目录）；" +
		"glob=只搜文件名匹配该模式的文件（如 *.go）；limit=最多返回多少条匹配（默认 100）。" +
		"只读，不改任何文件。先 grep 定位，再用 read 看上下文；" +
		"改代码前请先 read 确认当前内容（写前观察门会拦）。"
}

type grepParams struct {
	Pattern string `json:"pattern" mos:"desc=正则表达式（RE2 语法），必填"`
	Path    string `json:"path,omitempty" mos:"desc=搜索目录或文件路径（相对路径以工作目录为锚点），默认工作目录"`
	Glob    string `json:"glob,omitempty" mos:"desc=只搜文件名匹配该模式的文件，如 *.go"`
	Limit   int    `json:"limit,omitempty" mos:"desc=最多返回多少条匹配，默认 100"`
}

func (Grep) Parameters() any { return &grepParams{} }

// ConcurrencySafe 实现 tool.ConcurrencySafe：纯只读，可并发。
func (Grep) ConcurrencySafe(json.RawMessage) bool { return true }

// Execute 实现 tool.Tool.Execute。
func (g *Grep) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[grepParams](args)
	if err != nil {
		return nil, err
	}
	if p.Pattern == "" {
		return nil, fmt.Errorf("pattern 不能为空")
	}
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern 不是合法正则（RE2）：%w；如需字面量匹配请自行转义特殊字符", err)
	}
	if g.FS == nil {
		return nil, &filesystem.NotConfiguredError{}
	}
	res, err := g.FS.Resolve(ctx, filesystem.Request{Path: p.Path, Op: filesystem.OpRead})
	if err != nil {
		return nil, err
	}
	limit := clampLimit(p.Limit, defaultGrepLimit, maxGrepLimit)

	var filter *globMatcher
	if p.Glob != "" {
		filter, err = newGlobMatcher(p.Glob)
		if err != nil {
			return nil, err
		}
	}

	matches := make([]string, 0, 16)
	scanned := 0
	truncated := false

	// 单文件：只搜它自己。
	if !isDirPath(res.TargetKey) {
		scanned++
		if (filter == nil || filter.match(filepath.Base(res.TargetKey))) && !g.excluded(res.TargetKey) {
			lines, err := scanFile(res.TargetKey, re, limit)
			if err != nil {
				return nil, err
			}
			for _, ln := range lines {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", res.DisplayPath, ln.no, ln.text))
				if len(matches) >= limit {
					truncated = true
					break
				}
			}
		}
		return map[string]any{
			"matches":       matches,
			"count":         len(matches),
			"truncated":     truncated,
			"files_scanned": scanned,
		}, nil
	}

	// 目录：逐文件扫。
	err = walkFiles(res.TargetKey, func(path, rel string) error {
		if len(matches) >= limit {
			truncated = true
			return errStopWalk
		}
		if scanned >= maxSearchFiles {
			truncated = true
			return errStopWalk
		}
		scanned++
		if filter != nil && !filter.match(rel) {
			return nil
		}
		if g.excluded(path) {
			return nil
		}
		lines, err := scanFile(path, re, limit-len(matches))
		if err != nil {
			return nil // 单个文件读不动（权限/编码）：跳过，不中断整次搜索
		}
		for _, ln := range lines {
			matches = append(matches, fmt.Sprintf("%s:%d:%s", path, ln.no, ln.text))
		}
		return nil
	})
	if err != nil && err != errStopWalk {
		return nil, err
	}
	// 命中数达到上限即标 truncated：可能还有更多（保守标注，不假装"全找到了"）。
	if len(matches) >= limit {
		truncated = true
	}
	return map[string]any{
		"matches":       matches,
		"count":         len(matches),
		"truncated":     truncated,
		"files_scanned": scanned,
	}, nil
}

// excluded 报告文件是否被宿主策略排除（Exclude 为 nil 时一律不排除）。
func (g *Grep) excluded(path string) bool {
	return g != nil && g.Exclude != nil && g.Exclude(path)
}

// matchLine 是一条匹配：行号（1 起）+ 行文本。
type matchLine struct {
	no   int
	text string
}

// scanFile 扫描单个文件的匹配行（最多返回 limit 条）。
//
// 只扫文件前 maxSearchFileBytes 字节：大文件只看前缀（少见的超长日志），
// 命中上限按 limit 收口。返回的每行已去行尾、已按 maxLineChars 截断。
func scanFile(path string, re *regexp.Regexp, limit int) ([]matchLine, error) {
	if limit <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 二进制嗅探：含 NUL 即判为二进制（跳过——模型读不懂，grep 出来也是噪声）。
	sniff := make([]byte, binarySniffBytes)
	n, _ := io.ReadFull(f, sniff)
	if n == 0 {
		return nil, nil
	}
	if strings.IndexByte(string(sniff[:n]), 0) >= 0 {
		return nil, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var out []matchLine
	br := bufio.NewReader(f)
	read := 0
	for no := 1; ; no++ {
		line, rerr := br.ReadString('\n')
		if rerr != nil && rerr != io.EOF {
			return out, nil
		}
		if len(line) > 0 {
			read += len(line)
			if read > maxSearchFileBytes {
				break // 只扫前缀
			}
			text := strings.TrimRight(line, "\r\n")
			if re.MatchString(text) {
				out = append(out, matchLine{no: no, text: truncateLongLine(text)})
				if len(out) >= limit {
					break
				}
			}
		}
		if rerr == io.EOF {
			break
		}
	}
	return out, nil
}

// Glob 按 glob 模式找出匹配的文件（PI 的 find / DSH 的 glob 对位）。
//
// 模式语义：
//   - `*`  单层内任意字符（不含路径分隔符）；`**` 跨任意层目录；
//   - `?`  单层内任意单字符；
//   - 不含 "/" 的模式按**文件名**匹配（任意层级都算，如 *.go）；
//     含 "/" 的模式按**相对搜索根的路径**匹配（src/**/*.go）。
type Glob struct {
	FS filesystem.FileSystem
	// Exclude 是**宿主策略**的排除谓词：返回 true 的文件不出现在结果里。
	// 与 Grep.Exclude 同理（策略归宿主，框架只提供挂点）。nil = 不排除。
	Exclude func(path string) bool
}

// NewGlob 构造文件名匹配工具。
func NewGlob(fsys filesystem.FileSystem) *Glob { return &Glob{FS: fsys} }

func (Glob) Name() string { return "glob" }

func (Glob) Description() string {
	return "按 glob 模式查找文件，返回匹配的文件路径（受工作目录沙箱限制）。" +
		"参数：pattern=glob 模式（必填），如 **/*.go、src/**/*.ts、*.md；" +
		"path=搜索起始目录（默认工作目录）；limit=最多返回多少个（默认 1000）。" +
		"只返回文件（不含目录）；只想看某个目录的内容用 ls。"
}

type globParams struct {
	Pattern string `json:"pattern" mos:"desc=glob 模式，如 **/*.go，必填"`
	Path    string `json:"path,omitempty" mos:"desc=搜索起始目录（相对路径以工作目录为锚点），默认工作目录"`
	Limit   int    `json:"limit,omitempty" mos:"desc=最多返回多少个匹配，默认 1000"`
}

func (Glob) Parameters() any { return &globParams{} }

// ConcurrencySafe 实现 tool.ConcurrencySafe：纯只读，可并发。
func (Glob) ConcurrencySafe(json.RawMessage) bool { return true }

// Execute 实现 tool.Tool.Execute。
func (gl *Glob) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[globParams](args)
	if err != nil {
		return nil, err
	}
	if p.Pattern == "" {
		return nil, fmt.Errorf("pattern 不能为空")
	}
	matcher, err := newGlobMatcher(p.Pattern)
	if err != nil {
		return nil, err
	}
	if gl.FS == nil {
		return nil, &filesystem.NotConfiguredError{}
	}
	res, err := gl.FS.Resolve(ctx, filesystem.Request{Path: p.Path, Op: filesystem.OpRead})
	if err != nil {
		return nil, err
	}
	if !isDirPath(res.TargetKey) {
		return nil, fmt.Errorf("%q 不是目录（glob 需要在目录里查找）", res.DisplayPath)
	}
	limit := clampLimit(p.Limit, defaultGlobLimit, maxGlobLimit)

	files := make([]string, 0, 32)
	truncated := false
	err = walkFiles(res.TargetKey, func(path, rel string) error {
		if len(files) >= limit {
			truncated = true
			return errStopWalk
		}
		if matcher.match(rel) && !gl.excluded(path) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil && err != errStopWalk {
		return nil, err
	}
	return map[string]any{
		"files":     files,
		"count":     len(files),
		"truncated": truncated,
	}, nil
}

// excluded 报告文件是否被宿主策略排除（Exclude 为 nil 时一律不排除）。
func (gl *Glob) excluded(path string) bool {
	return gl != nil && gl.Exclude != nil && gl.Exclude(path)
}

// errStopWalk 是遍历提前结束的哨兵错误（不是失败）。
var errStopWalk = fmt.Errorf("builtin: 遍历提前结束")

// walkFiles 遍历 root 下的**常规文件**（不跟随符号链接）。
//
// rel 是相对 root 的斜杠路径（用于模式匹配与展示）。
func walkFiles(root string, fn func(path, rel string) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// 读不了的目录/条目：跳过，不中断整次搜索。
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skippedDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// 符号链接、设备、管道等一律不碰（不跟随、不打开）。
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		return fn(p, filepath.ToSlash(rel))
	})
}

// globMatcher 是编译好的 glob 模式。
//
// 编译成两条正则：一条按相对路径匹配，一条按文件名匹配（不含 "/" 的模式
// 靠后者命中任意层级）。
type globMatcher struct {
	re *regexp.Regexp
}

// newGlobMatcher 编译 glob 模式。
func newGlobMatcher(pattern string) (*globMatcher, error) {
	re, err := compileGlob(pattern)
	if err != nil {
		return nil, err
	}
	return &globMatcher{re: re}, nil
}

// match 报告相对路径 rel 是否命中（先按相对路径试，再按文件名试——
// 让 "*.go" 命中任意层级的 .go 文件，让 "src/**" 命中子目录）。
func (m *globMatcher) match(rel string) bool {
	if m == nil || m.re == nil {
		return true
	}
	if m.re.MatchString(rel) {
		return true
	}
	return m.re.MatchString(pathBase(rel))
}

// compileGlob 把 glob 模式编译成正则：
//
//	**  →  跨任意层目录（`**/` → `(?:[^/]+/)*`，可为空层）
//	*   →  单层内任意字符（`[^/]*`）
//	?   →  单层内任意单字符（`[^/]`）
//	[…] →  字符组（`!`/`^` 开头表示取反，对齐 shell 习惯）
//	{a,b} →  择一（花括号展开）
//	其余按字面量（正则元字符被转义）
//
// 语法够用即可：模型常用的是 `**`、`*`、`?` 与字符组；不实现的是
// 扩展 glob（`+(a|b)`、`@(...)`）——遇到就当字面量。
func compileGlob(pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, fmt.Errorf("pattern 不能为空")
	}
	var b strings.Builder
	b.WriteString("^")
	n := len(pattern)
	for i := 0; i < n; i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < n && pattern[i+1] == '*' {
				j := i
				for j < n && pattern[j] == '*' {
					j++
				}
				if j < n && pattern[j] == '/' {
					// `**/` → 任意层目录（含 0 层）
					b.WriteString("(?:[^/]+/)*")
					j++
				} else {
					b.WriteString(".*")
				}
				i = j - 1
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			class, next, ok := globCharClass(pattern, i)
			if !ok {
				b.WriteString(regexp.QuoteMeta("["))
				continue
			}
			b.WriteString(class)
			i = next - 1
		case '{':
			alts, next, ok := globBraces(pattern, i)
			if !ok {
				b.WriteString(regexp.QuoteMeta("{"))
				continue
			}
			b.WriteString("(?:")
			b.WriteString(strings.Join(alts, "|"))
			b.WriteString(")")
			i = next - 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("glob 模式 %q 无效: %w", pattern, err)
	}
	return re, nil
}

// globCharClass 解析字符组 `[...]`，返回正则片段与结束位置。
// 未闭合或内容为空 → ok=false（调用方按字面量处理）。
func globCharClass(pattern string, start int) (string, int, bool) {
	n := len(pattern)
	i := start + 1
	negate := false
	if i < n && (pattern[i] == '!' || pattern[i] == '^') {
		negate = true
		i++
	}
	var body strings.Builder
	for i < n && pattern[i] != ']' {
		if pattern[i] == '\\' && i+1 < n {
			body.WriteString(regexp.QuoteMeta(string(pattern[i+1])))
			i += 2
			continue
		}
		body.WriteString(string(pattern[i]))
		i++
	}
	if i >= n || body.Len() == 0 {
		return "", start, false
	}
	cls := "[" + body.String() + "]"
	if negate {
		cls = "[^" + body.String() + "]"
	}
	return cls, i + 1, true
}

// globBraces 解析 `{a,b}` 择一，返回各分支与结束位置。
// 未闭合 → ok=false（按字面量处理）。
func globBraces(pattern string, start int) ([]string, int, bool) {
	n := len(pattern)
	depth := 0
	var (
		parts []string
		cur   strings.Builder
	)
	for i := start; i < n; i++ {
		switch pattern[i] {
		case '{':
			depth++
			if depth == 1 {
				continue
			}
		case '}':
			depth--
			if depth == 0 {
				parts = append(parts, cur.String())
				if len(parts) == 1 {
					return nil, start, false // 无择一（{} 或 {a}）→ 字面量
				}
				return parts, i + 1, true
			}
		case ',':
			if depth == 1 {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(pattern[i])
	}
	return nil, start, false
}

// pathBase 取斜杠路径的末段（跨平台安全）。
func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// clampLimit 把模型给的 limit 夹到 [1, max]（<=0 用默认值）。
func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// isDirPath 判断路径是否为目录（不存在也算 false）。
func isDirPath(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
