package search

import (
	"context"
	"net/http"
)

// tavilyEndpoint 是 Tavily Search API 地址（var 便于测试覆盖）。
var tavilyEndpoint = "https://api.tavily.com/search"

// TavilyProvider 通过 Tavily Search API 搜索，需 API Key。
type TavilyProvider struct {
	APIKey string
}

func (p *TavilyProvider) Name() string { return "tavily" }

func (p *TavilyProvider) Search(ctx context.Context, query string, numResults int) ([]Result, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	req, err := newRequest(ctx, http.MethodPost, tavilyEndpoint, map[string]any{
		"api_key":     p.APIKey,
		"query":       query,
		"max_results": numResults,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := doJSON(req, &resp); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}
