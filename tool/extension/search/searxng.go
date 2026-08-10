package search

import (
	"context"
	"net/http"
	"net/url"
)

// SearXNGProvider 通过自托管 SearXNG 实例搜索（JSON 格式，需在 SearXNG 配置中启用 format=json）。
type SearXNGProvider struct {
	BaseURL string
}

func (p *SearXNGProvider) Name() string { return "searxng" }

func (p *SearXNGProvider) Search(ctx context.Context, query string, numResults int) ([]Result, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	u := p.BaseURL + "/search?q=" + url.QueryEscape(query) + "&format=json"
	req, err := newRequest(ctx, http.MethodGet, u, nil)
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
	if len(out) > numResults {
		out = out[:numResults]
	}
	return out, nil
}
