// Package search 提供网络搜索的提供商抽象与实现。
//
// 支持的提供商：
//   - duckduckgo：免费，解析 DuckDuckGo HTML 结果页，无需 API Key（默认）
//   - tavily / serper / brave：需 API Key
//   - searxng：自托管 SearXNG，提供 base_url 即可
//
// 所有提供商统一返回 Result 结构；装配信息见 internal/config 的 MOS_SEARCH_* 约定。
package search

import (
	"context"
	"errors"
	"fmt"
)

// Result 是一条搜索结果。
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Provider 是搜索提供商。
type Provider interface {
	// Name 返回提供商名称（duckduckgo / tavily / serper / brave / searxng）。
	Name() string
	// Search 搜索 query 并返回最多 numResults 条结果。
	Search(ctx context.Context, query string, numResults int) ([]Result, error)
}

// Config 描述搜索提供商的装配信息。
type Config struct {
	// Provider 是提供商名称；空表示 duckduckgo。
	Provider string
	// Region 是 duckduckgo 的区域参数（如 cn-cn、us-en）；空使用默认区域。
	Region string
	// APIKey 是 tavily / serper / brave 的密钥。
	APIKey string
	// BaseURL 是 searxng 的地址，或 duckduckgo 的自定义端点。
	BaseURL string
}

// New 按配置构建搜索提供商。提供商未知或缺少必需配置（如 API Key）时报错。
func New(cfg Config) (Provider, error) {
	switch cfg.Provider {
	case "", "duckduckgo":
		return &DuckDuckGoProvider{Region: cfg.Region, Endpoint: cfg.BaseURL}, nil
	case "tavily":
		if cfg.APIKey == "" {
			return nil, errors.New("search: tavily 需要 API Key（MOS_SEARCH_API_KEY）")
		}
		return &TavilyProvider{APIKey: cfg.APIKey}, nil
	case "serper":
		if cfg.APIKey == "" {
			return nil, errors.New("search: serper 需要 API Key（MOS_SEARCH_API_KEY）")
		}
		return &SerperProvider{APIKey: cfg.APIKey}, nil
	case "brave":
		if cfg.APIKey == "" {
			return nil, errors.New("search: brave 需要 API Key（MOS_SEARCH_API_KEY）")
		}
		return &BraveProvider{APIKey: cfg.APIKey}, nil
	case "searxng":
		if cfg.BaseURL == "" {
			return nil, errors.New("search: searxng 需要 base_url（MOS_SEARCH_BASE_URL）")
		}
		return &SearXNGProvider{BaseURL: cfg.BaseURL}, nil
	default:
		return nil, fmt.Errorf("search: 未知提供商 %q（支持 duckduckgo/tavily/serper/brave/searxng）", cfg.Provider)
	}
}
