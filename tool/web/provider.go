package web

import (
	"context"
	"fmt"
	"sync"
)

// ── 归一化词汇表（能力面） ──
//
// 形状对齐 DSH 的 ctx.web（packages/web/web/src/types.ts）：**可选字段缺失就
// 缺，别编**——provider 没给标题就空着，让模型看见"只有 URL"，比编一个假标题
// 诚实得多。

// Source 是一条可引用的来源。
type Source struct {
	// URL 是来源地址（必填——没有 URL 的来源不可引用）。
	URL string `json:"url"`
	// Title 是标题（可选）。
	Title string `json:"title,omitempty"`
	// Snippet 是摘要（可选）。
	Snippet string `json:"snippet,omitempty"`
	// PublishedAt 是发布时间（可选，provider 原样给的字符串）。
	PublishedAt string `json:"published_at,omitempty"`
}

// SearchResult 是一次搜索的结果。
type SearchResult struct {
	// Summary 是 provider 生成的答案摘要（可选：有些厂商只给来源列表）。
	Summary string `json:"summary,omitempty"`
	// Note 是给模型看的附注（可选：比如"这几条查询失败了"）——与 Summary
	// 分开：Summary 是**别人的答案**，Note 是**我们的说明**，别混。
	Note string `json:"note,omitempty"`
	// Sources 是来源列表。
	Sources []Source `json:"sources"`
	// Truncated 标记来源被截断（到上限）。
	Truncated bool `json:"truncated,omitempty"`
}

// FetchResult 是一次抓取的结果。
//
// 语义（对齐 DSH）：**非 2xx 是结果，不是错误**——404 是"这个资源的状态"，
// 不是"抓取失败"；抓取失败（DNS 失败、被拦、超时）才是错误。
type FetchResult struct {
	// URL 是最终地址（跟随允许的重定向后）。
	URL string `json:"url"`
	// StatusCode 是 HTTP 状态码。
	StatusCode int `json:"status_code"`
	// Title 是页面标题（可选）。
	Title string `json:"title,omitempty"`
	// ContentType 是响应的内容类型（可选）。
	ContentType string `json:"content_type,omitempty"`
	// Content 是转成纯文本后的正文。
	Content string `json:"content"`
	// Truncated 标记正文被截断。
	Truncated bool `json:"truncated,omitempty"`
}

// ── 能力接口 ──

// Searcher 是模型面依赖的搜索能力（宿主注入）。
//
// 语义：**一次调用带多条查询**（并行搜、合并去重），返回归一化结果。
type Searcher interface {
	// Name 是后端名（诊断用：拼进错误信息）。
	Name() string
	// Search 搜索并返回来源；limit 是来源条数上限。
	Search(ctx context.Context, queries []string, limit int) (SearchResult, error)
}

// Fetcher 是模型面依赖的抓取能力（宿主注入）。
type Fetcher interface {
	// Name 是后端名（诊断用）。
	Name() string
	// Fetch 抓取一个 URL 并返回归一化结果。
	Fetch(ctx context.Context, url string) (FetchResult, error)
}

// Provider 是**单条查询**的搜索后端（内置适配器实现它）。
//
// 单条形状是刻意的：厂商 API 基本都是"一次一个 query"，多查询合并是上层的
// 责任（见 NewSearcher）。
type Provider interface {
	// Name 是后端名。
	Name() string
	// Search 搜一条查询，返回至多 limit 条来源。
	Search(ctx context.Context, query string, limit int) ([]Source, error)
}

// NewSearcher 把单查询的 Provider 包成多查询的 Searcher：
// 多条查询**并行**发出（各自独立），结果按 URL 去重合并，最后截到 limit。
//
// 并行是刻意的：4 条查询串行要等 4 个 RTT；并行后只等最慢的那条。
func NewSearcher(p Provider) Searcher {
	if p == nil {
		return nil
	}
	return &multiSearcher{provider: p}
}

// multiSearcher 是 Provider 的多查询包装。
type multiSearcher struct {
	provider Provider
}

// Name 实现 Searcher。
func (m *multiSearcher) Name() string { return m.provider.Name() }

// Search 实现 Searcher：并行搜 → 合并去重 → 截到 limit。
func (m *multiSearcher) Search(ctx context.Context, queries []string, limit int) (SearchResult, error) {
	if len(queries) == 0 {
		return SearchResult{}, fmt.Errorf("web: 没有查询")
	}
	if limit <= 0 {
		limit = DefaultMaxResults
	}
	results := make([][]Source, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			results[i], errs[i] = m.provider.Search(ctx, q, limit)
		}(i, q)
	}
	wg.Wait()

	// 汇总错误：部分失败不算全失败——有结果就用结果，并如实说明哪条查询失败。
	var firstErr error
	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, queries[i])
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	var merged []Source
	seen := map[string]bool{}
	for _, rs := range results {
		for _, s := range rs {
			if s.URL == "" || seen[s.URL] {
				continue
			}
			seen[s.URL] = true
			merged = append(merged, s)
		}
	}
	truncated := false
	if len(merged) > limit {
		merged = merged[:limit]
		truncated = true
	}
	if len(merged) == 0 && firstErr != nil {
		return SearchResult{}, fmt.Errorf("web: 全部查询都失败（%s）: %w", queries[0], firstErr)
	}
	res := SearchResult{Sources: merged, Truncated: truncated}
	if len(failed) > 0 {
		res.Note = fmt.Sprintf("以下查询失败：%v", failed)
	}
	return res, nil
}
