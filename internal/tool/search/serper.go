package search

import (
	"context"
	"net/http"
)

// serperEndpoint 是 Serper（Google 搜索）API 地址（var 便于测试覆盖）。
var serperEndpoint = "https://google.serper.dev/search"

// SerperProvider 通过 Serper Google Search API 搜索，需 API Key。
type SerperProvider struct {
	APIKey string
}

func (p *SerperProvider) Name() string { return "serper" }

func (p *SerperProvider) Search(ctx context.Context, query string, numResults int) ([]Result, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	req, err := newRequest(ctx, http.MethodPost, serperEndpoint, map[string]any{
		"q":   query,
		"num": numResults,
	})
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", p.APIKey)
	var resp struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := doJSON(req, &resp); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(resp.Organic))
	for _, r := range resp.Organic {
		out = append(out, Result{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return out, nil
}
