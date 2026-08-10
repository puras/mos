package search

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// braveEndpoint 是 Brave Search API 地址（var 便于测试覆盖）。
var braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

// BraveProvider 通过 Brave Search API 搜索，需 API Key。
type BraveProvider struct {
	APIKey string
}

func (p *BraveProvider) Name() string { return "brave" }

func (p *BraveProvider) Search(ctx context.Context, query string, numResults int) ([]Result, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	req, err := newRequest(ctx, http.MethodGet,
		braveEndpoint+"?q="+url.QueryEscape(query)+"&count="+strconv.Itoa(numResults), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+p.APIKey)
	var resp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := doJSON(req, &resp); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(resp.Web.Results))
	for _, r := range resp.Web.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return out, nil
}
