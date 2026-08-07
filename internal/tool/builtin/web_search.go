package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"hegq.net/mos/internal/tool/search"
	"hegq.net/mos/pkg/tool"
)

// WebSearch 搜索互联网获取实时信息，返回标题、链接与摘要列表。
// Provider 指定搜索提供商（可用 search.New / internal/config 装配）；空时使用默认 DuckDuckGo。
type WebSearch struct {
	Provider search.Provider
}

type searchParams struct {
	Query      string `json:"query" mos:"desc=搜索关键词"`
	MaxResults int    `json:"max_results,omitempty" mos:"desc=返回结果条数，默认 5，最大 10"`
}

func (s WebSearch) Name() string { return "web_search" }
func (s WebSearch) Description() string {
	return "搜索互联网获取实时信息，返回结果标题、链接与摘要列表"
}
func (s WebSearch) Parameters() any { return &searchParams{} }
func (s WebSearch) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[searchParams](args)
	if err != nil {
		return nil, err
	}
	p.Query = strings.TrimSpace(p.Query)
	if p.Query == "" {
		return nil, errors.New("查询关键词不能为空")
	}
	max := p.MaxResults
	if max <= 0 {
		max = 5
	}
	if max > 10 {
		max = 10
	}

	sp := s.Provider
	if sp == nil {
		sp = &search.DuckDuckGoProvider{}
	}
	results, err := sp.Search(ctx, p.Query, max)
	if err != nil {
		return nil, fmt.Errorf("搜索失败: %w", err)
	}
	return map[string]any{
		"query":   p.Query,
		"results": results,
	}, nil
}

var _ tool.Tool = WebSearch{}
