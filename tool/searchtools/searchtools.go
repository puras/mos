// Package searchtools 提供 tool_search：deferred 工具的发现与激活。
//
// 机制对齐 PI 的 tool_search（packages/coding-agent/src/extensions/tool-search/tool.ts）：
// deferred 工具只把名字与描述暴露给本工具，模型按需搜索，命中即激活——
// 下一轮请求才把这些工具声明给模型（协议无关：不依赖 Anthropic 的
// defer_loading，MoS 三协议适配器都不用改）。BM25 排序参数与分词规则
// （camelCase 切分、停用词、简单词干）逐项移植自 PI。
//
// 纪律（AGENTS.md 功能分析纪律）的完整五行分析见 docs/ARCHITECTURE.md §3.15。
package searchtools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/puras/mos/tool"
)

// ToolSearchName 是发现工具的名字。
const ToolSearchName = "tool_search"

// DefaultLimit 是单次搜索返回并激活的最大工具数（PI 同款默认值）。
const DefaultLimit = 8

// ── BM25（Okapi，k1=1.2 / b=0.75，PI 同参数）──

// stopWords 是检索停用词（PI 同表）。
var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "in": true, "is": true,
	"it": true, "of": true, "on": true, "or": true, "that": true, "the": true,
	"this": true, "to": true, "with": true,
}

// tokenize 分词：camelCase/ snake_case 切开、小写、去停用词、简单词干还原。
// 移植 PI 的 tokenize（tool.ts:64-79）并扩展 CJK：连续汉字按**重叠二元组**
// （bigram）成词元（"搜索资料" → [搜索, 索资, 资料]）——中文检索的标准做法：
// 单字词元区分度太低（一个"网"字让无关工具也命中），bigram 无需分词词典
// 即有足够的精度。拉丁侧与 PI 一致：camelCase 边界切分 + 停用词 + 简单词干。
func tokenize(text string) []string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r >= 'A' && r <= 'Z':
			// camelCase 边界：大写字母前补空格——与 PI 的两步正则等效（多切无害）。
			b.WriteByte(' ')
			b.WriteRune(r + ('a' - 'A'))
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case isCJK(r):
			b.WriteRune(r) // 原样保留，后面按 bigram 切
		default:
			b.WriteByte(' ') // snake_case、标点、空白：一律当分隔符
		}
	}
	var out []string
	for term := range strings.SplitSeq(b.String(), " ") {
		if term == "" {
			continue
		}
		out = append(out, cjkOrLatin(term)...)
	}
	filtered := out[:0]
	for _, t := range out {
		if !stopWords[t] {
			filtered = append(filtered, t)
		}
	}
	for i := range filtered {
		filtered[i] = stem(filtered[i])
	}
	return filtered
}

// cjkOrLatin 把一个无空格词元切成词元序列：纯拉丁按原样（再去停用词/词干
// 在调用侧）；含 CJK 的按重叠二元组。
func cjkOrLatin(term string) []string {
	runes := []rune(term)
	if !isCJK(runes[0]) {
		return []string{term}
	}
	if len(runes) == 1 {
		return []string{term}
	}
	out := make([]string, 0, len(runes)-1)
	for i := 0; i+1 < len(runes); i++ {
		out = append(out, string(runes[i:i+2]))
	}
	return out
}

// isCJK 报告 r 是否中日韩统一表意文字（含扩展 A；不含假名/谚文——工具面
// 不会用它们写描述，真出现了当分隔符处理也无害）。
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // CJK 统一表意文字
		return true
	case r >= 0x3400 && r <= 0x4DBF: // 扩展 A
		return true
	default:
		return false
	}
}

// stem 简单词干还原（PI 同款：ies→y、ches/shes/sses/xes/zes→去 es、其余去尾 s）。
func stem(term string) string {
	switch {
	case len(term) > 4 && strings.HasSuffix(term, "ies"):
		return term[:len(term)-3] + "y"
	case len(term) > 4 && (strings.HasSuffix(term, "ches") || strings.HasSuffix(term, "shes") ||
		strings.HasSuffix(term, "sses") || strings.HasSuffix(term, "xes") || strings.HasSuffix(term, "zes")):
		return term[:len(term)-2]
	case len(term) > 3 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss"):
		return term[:len(term)-1]
	}
	return term
}

// doc 是一个工具的检索文档（PI 的 createToolSearchDocument 同源：名字 + 描述）。
//
// 与 PI 的差异：MoS 的 DeferredInfo 只带名字与描述，schema 属性名不参与——
// schema 本来就还没生成给模型，且描述里写清楚用途是工具作者的义务。
func doc(info tool.DeferredInfo) string {
	parts := []string{info.Name, strings.ReplaceAll(info.Name, "_", " "), info.Description}
	return strings.Join(parts, " ")
}

// rank 按 BM25 给文档打分排序，取前 limit 个（平分时保持注册序——稳定性同 PI）。
func rank(query string, docs []tool.DeferredInfo, limit int) []tool.DeferredInfo {
	if limit <= 0 || len(docs) == 0 {
		return nil
	}
	queryTerms := dedup(tokenize(query))
	if len(queryTerms) == 0 {
		return nil
	}
	tf := make([]map[string]int, len(docs))
	lengths := make([]float64, len(docs))
	var avgLen float64
	for i, d := range docs {
		terms := tokenize(doc(d))
		counts := make(map[string]int, len(terms))
		for _, t := range terms {
			counts[t]++
		}
		tf[i] = counts
		lengths[i] = float64(len(terms))
		avgLen += lengths[i]
	}
	avgLen /= float64(len(docs))
	if avgLen == 0 {
		avgLen = 1
	}
	k1, b := 1.2, 0.75
	n := float64(len(docs))
	idf := make(map[string]float64, len(queryTerms))
	for _, t := range queryTerms {
		df := 0
		for i := range docs {
			if tf[i][t] > 0 {
				df++
			}
		}
		idf[t] = log1p((n - float64(df) + 0.5) / (float64(df) + 0.5))
	}
	type scored struct {
		info  tool.DeferredInfo
		score float64
	}
	matches := make([]scored, 0, len(docs))
	for i := range docs {
		var score float64
		for _, t := range queryTerms {
			count := float64(tf[i][t])
			if count == 0 {
				continue
			}
			norm := k1 * (1 - b + b*lengths[i]/avgLen)
			score += idf[t] * (count * (k1 + 1) / (count + norm))
		}
		if score > 0 {
			matches = append(matches, scored{info: docs[i], score: score})
		}
	}
	// 分数降序；同分保持原序（slice.SliceStable + 显式比较）
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]tool.DeferredInfo, len(matches))
	for i, m := range matches {
		out[i] = m.info
	}
	return out
}

func log1p(x float64) float64 {
	return math.Log(1 + x)
}

func dedup(terms []string) []string {
	seen := make(map[string]bool, len(terms))
	out := terms[:0]
	for _, t := range terms {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// ── 工具定义 ──

// searchArgs 是 tool_search 的参数。
type searchArgs struct {
	Query string `json:"query" mos:"desc=要找什么（用自然语言描述用途）,required=true"`
	Limit int    `json:"limit,omitempty" mos:"desc=最多返回并激活几个工具（默认 8）"`
}

// Options 是 NewToolSearch 的参数。
type Options struct {
	// Registry 是宿主的工具注册表（搜索并激活它的 deferred 集）。
	Registry *tool.Registry
	// Limit 是默认返回上限。0 = DefaultLimit。
	Limit int
}

type toolSearch struct {
	reg   *tool.Registry
	limit int
}

func (t *toolSearch) Name() string { return ToolSearchName }

// Description 不枚举可搜的工具（PI 同款）：工具注册变化时描述保持不变，
// 利于 prompt cache；可搜什么由模型发起搜索获知。
func (t *toolSearch) Description() string {
	return "# 工具发现\n\n" +
		"在尚未加载的工具元数据上做检索（BM25），并把命中的工具激活供你下一轮调用。\n\n" +
		"有些工具（例如 MCP 服务器提供的）可能没有预先提供给你；需要时用本工具（" + ToolSearchName + "）" +
		"按用途搜索。搜索结果在下一轮起可用；回执会列出激活的工具。\n" +
		"参数 query 描述你要什么（不是工具名精确匹配）；limit 限制激活数量（默认 " +
		fmt.Sprint(t.limit) + "）。"
}

func (t *toolSearch) Parameters() any { return searchArgs{} }

func (t *toolSearch) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	input, err := tool.Parse[searchArgs](args)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Query) == "" {
		return nil, fmt.Errorf("query 不能为空")
	}
	limit := t.limit
	if input.Limit > 0 {
		limit = input.Limit
	}
	candidates := t.reg.DeferredTools()
	matches := rank(input.Query, candidates, limit)
	if len(matches) == 0 {
		return map[string]any{
			"found":  false,
			"loaded": []string{},
			"text":   "没有匹配的工具。换一种描述再试，或确认该能力是否属于本项目。",
		}, nil
	}
	names := make([]string, len(matches))
	lines := make([]string, len(matches))
	for i, m := range matches {
		names[i] = m.Name
		desc := firstLine(m.Description)
		lines[i] = fmt.Sprintf("- %s: %s", m.Name, desc)
	}
	activated, err := t.reg.Activate(names)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"found":  true,
		"loaded": activated,
		"text": fmt.Sprintf("已激活 %d 个工具，从你的下一轮调用起可用：\n%s",
			len(activated), strings.Join(lines, "\n")),
	}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// NewToolSearch 创建发现工具。deferred 集为空时它搜不到任何东西——
// 调用方（装配层）只在存在 deferred 工具时才注册它（能力即证据）。
func NewToolSearch(opts Options) tool.Tool {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	return &toolSearch{reg: opts.Registry, limit: limit}
}
