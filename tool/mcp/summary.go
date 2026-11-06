package mcp

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// 服务器摘要节（系统提示词里的「有哪些 MCP 服务器、怎么够到」）。
//
// 参照 PI 的 mcp_servers 节（packages/coding-agent/src/extensions/mcp/index.ts:156-224）：
// 只列**模型看不见**（deferred）的服务器——direct 的模型已经看得见，列了是
// 浪费；首行只讲「怎么够到」；每行 `命名空间 (tool_search): 一句话摘要`；
// 整节有预算，放不下就少列几个并写明还剩几个。
//
// 与 PI 的两处差异（各有理由）：
//   - **摘要只取宿主配置的 Description**，不取连接的 instructions——instructions
//     已在 user 侧一次性注入（连接后可能变，会破 prompt cache），再进静态
//     提示词就是重复且失真。
//   - **预算按字节算**（PI 按字符）——MoS 的其他预算（技能清单、说明注入）
//     一律按字节，与上下文预算口径一致。
const (
	// DefaultMaxServersSectionBytes 是服务器摘要节的默认字节上限（PI 同款 4096）。
	DefaultMaxServersSectionBytes = 4096
	// MaxServerDescriptionBytes 是单条服务器摘要的字节上限（PI 同款 250）。
	MaxServerDescriptionBytes = 250
)

// Summary 是一个服务器在摘要节里的条目。
type Summary struct {
	// Server 是服务器名（原始名，不带命名空间前缀）。
	Server string
	// Namespace 是给模型看的命名空间（mcp__<server>）——模型按它认出工具归属。
	Namespace string
	// Description 是宿主给的一句话摘要（可空）。
	Description string
}

// Summaries 返回**该进摘要节**的服务器：只列有 deferred 工具的。
//
// 口径（与 PI 的 hasIndirectTools 同款思路）：
//   - 全是 direct 的服务器 → 模型已经看得见，不列（列了是纯浪费）。
//   - 只有 hidden 的服务器 → 模型看不见也搜不到，列了模型也够不着，不列。
//   - 有 deferred 工具的 → 列（模型得知道它存在，才可能去搜）。
//
// 排序按服务器名（确定性输出 → prompt cache 稳定）。
func (m *Manager) Summaries() []Summary {
	var out []Summary
	for _, s := range m.Servers() {
		s.mu.RLock()
		tools := append([]*Tool(nil), s.tools...)
		s.mu.RUnlock()
		hasDeferred := false
		for _, t := range tools {
			if m.exposure(s.cfg, t.RawName()) == ExposureDeferred {
				hasDeferred = true
				break
			}
		}
		if !hasDeferred {
			continue
		}
		out = append(out, Summary{
			Server:      s.cfg.Name,
			Namespace:   "mcp__" + s.cfg.Name,
			Description: firstLine(s.cfg.Description),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out
}

// RenderServersSection 渲染服务器摘要节（进系统提示词）。
//
// 没有可列的服务器 → 返回空串（不加分节，不留空洞）。
// 预算放不下时：先砍服务器（按名字排序保留前 N 个），再砍描述（均分剩余
// 预算）；末尾写明还剩几个没列——模型据此知道「还有，但要用 tool_search」。
//
// **渲染层自己排序**（不指望调用方排）：输出必须确定，否则同一份配置两次
// 装配出的提示词不一样 → prompt cache 失效。
func RenderServersSection(summaries []Summary, maxBytes int) string {
	if len(summaries) == 0 {
		return ""
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxServersSectionBytes
	}
	sorted := append([]Summary(nil), summaries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Server < sorted[j].Server })
	const intro = "以下 MCP 服务器的工具不在你的清单里，用 tool_search 加载。"
	heads := make([]string, 0, len(sorted))
	for _, s := range sorted {
		heads = append(heads, fmt.Sprintf("- %s (tool_search)", s.Namespace))
	}
	omitted := func(count int) []string {
		if count <= 0 {
			return nil
		}
		return []string{fmt.Sprintf("- … 另有 %d 个（tool_search 可找）", count)}
	}
	// size(kept) = 首行 + 前 kept 个服务器行 + 省略行。
	//
	// **非单调**：砍掉服务器会多出一行「另有 N 个」，那行可能比省下的还贵
	//（kept=2 反而比 kept=3 更长）。所以不贪心递减，而是**枚举**取能放下的
	// 最大 kept（PI 的贪心版在这里会错过最优解）。
	size := func(kept int) int {
		parts := append([]string{intro}, heads[:kept]...)
		parts = append(parts, omitted(len(sorted)-kept)...)
		return len(strings.Join(parts, "\n"))
	}
	kept := 0
	for k := 0; k <= len(sorted); k++ {
		if size(k) <= maxBytes {
			kept = k
		}
	}
	if kept == 0 {
		// 连一行服务器都放不下：整节不渲染（提示词里留个空壳更糟）。
		return ""
	}
	// 描述预算：剩余字节均分给每个服务器（PI 同款算法）。减 2 是给
	// 「: 」分隔符留位——不减的话拼起来会超预算。
	perServer := 0
	if room := maxBytes - size(kept); kept > 0 && room > kept*2 {
		perServer = room/kept - 2
		if perServer > MaxServerDescriptionBytes {
			perServer = MaxServerDescriptionBytes
		}
	}
	lines := make([]string, 0, kept+1)
	for i := 0; i < kept; i++ {
		line := heads[i]
		if perServer > 0 && sorted[i].Description != "" {
			if desc := truncateBytes(sorted[i].Description, perServer); desc != "" {
				line += ": " + desc
			}
		}
		lines = append(lines, line)
	}
	lines = append(lines, omitted(len(sorted)-kept)...)
	return strings.Join(append([]string{intro}, lines...), "\n")
}

// firstLine 取文本第一行（PI 的 serverSummary 同款：描述是「一句话」）。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// truncateBytes 按字节上限截断（不切坏 UTF-8：按 rune 退到边界）。
//
// 末尾加省略号（占 3 字节）——截断了要让模型知道被截了。
func truncateBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 3 {
		return strings.TrimRight(s[:maxBytes], "")
	}
	cut := maxBytes - 3
	// 退到 rune 边界（UTF-8 续字节是 10xxxxxx）。
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
