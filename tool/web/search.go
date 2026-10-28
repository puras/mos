package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/puras/mos/httpx"
)

// 搜索厂商适配器（厂商面）。
//
// 五家适配器移植自旧实现（v2.24 的 tool/extension/search），本轮按新词汇表
// 收口：统一返回 []Source（可选字段缺失就缺，不编），并允许宿主指定端点
// （便于自托管与测试）。
//
// 网络出口走 httpx seam（宿主可注入统一代理/重试/审计的客户端）：搜索端点
// 由**配置**决定、不是模型给的 URL，所以这里没有 SSRF 面（web_fetch 才有）。

const (
	// searchMaxBytes 是单次搜索响应体上限（2MB），防异常响应撑爆内存。
	searchMaxBytes = 2 << 20
	// searchTimeout 是单次搜索的超时。
	searchTimeout = 30 * time.Second
	// searchUserAgent 是请求携带的 User-Agent（显式产品标识，不伪装浏览器）。
	searchUserAgent = "mos-web-search/1.0 (+https://github.com/puras/mos)"
)

// newRequest 构造携带 UA 的 GET/POST 请求（超时由 ctx 控制）。
func newRequest(ctx context.Context, method, rawURL string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", searchUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// doJSON 发起请求并把 JSON 响应解码到 out；非 200 状态码报错。
func doJSON(req *http.Request, out any) error {
	resp, err := httpx.FromContext(req.Context()).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, searchMaxBytes)).Decode(out)
}

// ── DuckDuckGo（零 Key，免费） ──

// DuckDuckGoProvider 通过 DuckDuckGo 的 HTML 结果页搜索——**不需要 API Key**。
type DuckDuckGoProvider struct {
	// Region 是区域参数（kl，如 cn-cn）；空用默认。
	Region string
	// SafeSearch 是安全级别（off | moderate | strict）；空为 moderate。
	SafeSearch string
	// Endpoint 是自定义端点；空用 html.duckduckgo.com。
	Endpoint string
}

// NewDuckDuckGo 构造 DuckDuckGo 搜索后端（零 Key）。
func NewDuckDuckGo() *DuckDuckGoProvider { return &DuckDuckGoProvider{} }

// Name 实现 Provider。
func (p *DuckDuckGoProvider) Name() string { return "duckduckgo" }

// duckduckgoDefaultEndpoint 是 DuckDuckGo HTML 版搜索地址。
const duckduckgoDefaultEndpoint = "https://html.duckduckgo.com/html/"

// Search 实现 Provider。
func (p *DuckDuckGoProvider) Search(ctx context.Context, query string, limit int) ([]Source, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = duckduckgoDefaultEndpoint
	}
	u, err := url.Parse(endpoint + "?q=" + url.QueryEscape(query))
	if err != nil {
		return nil, err
	}
	q := u.Query()
	if p.Region != "" {
		q.Set("kl", p.Region)
	}
	if kp := ddgSafeSearchKP(p.SafeSearch); kp != "" {
		q.Set("kp", kp)
	}
	u.RawQuery = q.Encode()

	req, err := newRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpx.FromContext(ctx).Do(req)
	if err != nil {
		return nil, fmt.Errorf("搜索失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, searchMaxBytes))
	if err != nil {
		return nil, err
	}
	return parseDuckDuckGo(body, limit), nil
}

// ddgSafeSearchKP 把安全级别映射成 DDG 的 kp 参数；未知值返回空（用默认）。
func ddgSafeSearchKP(level string) string {
	switch strings.ToLower(level) {
	case "off":
		return "-1"
	case "strict":
		return "-2"
	case "moderate", "":
		return "1"
	default:
		return ""
	}
}

var (
	ddgTitleRe   = regexp.MustCompile(`(?is)class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnipRe    = regexp.MustCompile(`(?is)class="result__snippet"[^>]*>(.*?)</a>`)
	ddgURLPrefix = "//duckduckgo.com/l/?uddg="
)

// parseDuckDuckGo 从结果页解析标题/链接/摘要，最多返回 limit 条。
func parseDuckDuckGo(body []byte, limit int) []Source {
	s := string(body)
	titles := ddgTitleRe.FindAllStringSubmatch(s, -1)
	snips := ddgSnipRe.FindAllStringSubmatch(s, -1)
	n := len(titles)
	if n > len(snips) {
		n = len(snips)
	}
	if limit > 0 && n > limit {
		n = limit
	}
	out := make([]Source, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Source{
			Title:   htmlToText(titles[i][2]),
			URL:     ddgCleanURL(titles[i][1]),
			Snippet: htmlToText(snips[i][1]),
		})
	}
	return out
}

// ddgCleanURL 把 DuckDuckGo 的跳转链接还原成目标地址；非跳转链接原样返回。
func ddgCleanURL(href string) string {
	href = strings.TrimSpace(href)
	if strings.HasPrefix(href, ddgURLPrefix) {
		if u, err := url.Parse(href); err == nil {
			if d := u.Query().Get("uddg"); d != "" {
				return d
			}
		}
	}
	return strings.TrimPrefix(href, "//")
}

// ── Tavily ──

// TavilyProvider 通过 Tavily Search API 搜索（需 API Key）。
type TavilyProvider struct {
	// APIKey 是 Tavily 密钥（空 = 不可用）。
	APIKey string
	// BaseURL 是 API 根地址；空用官方地址。
	BaseURL string
}

// NewTavily 构造 Tavily 搜索后端。
func NewTavily(apiKey string) *TavilyProvider { return &TavilyProvider{APIKey: apiKey} }

// Name 实现 Provider。
func (p *TavilyProvider) Name() string { return "tavily" }

// Search 实现 Provider。
func (p *TavilyProvider) Search(ctx context.Context, query string, limit int) ([]Source, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("tavily: 缺少 API Key")
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	base := p.BaseURL
	if base == "" {
		base = "https://api.tavily.com"
	}
	req, err := newRequest(ctx, http.MethodPost, base+"/search", map[string]any{
		"api_key":     p.APIKey,
		"query":       query,
		"max_results": limit,
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
	out := make([]Source, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, Source{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

// ── Serper（Google 搜索） ──

// SerperProvider 通过 Serper API 搜索（需 API Key）。
type SerperProvider struct {
	// APIKey 是 Serper 密钥（空 = 不可用）。
	APIKey string
	// BaseURL 是 API 根地址；空用官方地址。
	BaseURL string
}

// NewSerper 构造 Serper 搜索后端。
func NewSerper(apiKey string) *SerperProvider { return &SerperProvider{APIKey: apiKey} }

// Name 实现 Provider。
func (p *SerperProvider) Name() string { return "serper" }

// Search 实现 Provider。
func (p *SerperProvider) Search(ctx context.Context, query string, limit int) ([]Source, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("serper: 缺少 API Key")
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	base := p.BaseURL
	if base == "" {
		base = "https://google.serper.dev"
	}
	req, err := newRequest(ctx, http.MethodPost, base+"/search", map[string]any{
		"q":   query,
		"num": limit,
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
	out := make([]Source, 0, len(resp.Organic))
	for _, r := range resp.Organic {
		out = append(out, Source{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return out, nil
}

// ── Brave ──

// BraveProvider 通过 Brave Search API 搜索（需 API Key）。
type BraveProvider struct {
	// APIKey 是 Brave 密钥（空 = 不可用）。
	APIKey string
	// BaseURL 是 API 根地址；空用官方地址。
	BaseURL string
}

// NewBrave 构造 Brave 搜索后端。
func NewBrave(apiKey string) *BraveProvider { return &BraveProvider{APIKey: apiKey} }

// Name 实现 Provider。
func (p *BraveProvider) Name() string { return "brave" }

// Search 实现 Provider。
func (p *BraveProvider) Search(ctx context.Context, query string, limit int) ([]Source, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("brave: 缺少 API Key")
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	base := p.BaseURL
	if base == "" {
		base = "https://api.search.brave.com/res/v1/web/search"
	}
	req, err := newRequest(ctx, http.MethodGet,
		base+"?q="+url.QueryEscape(query)+"&count="+strconv.Itoa(limit), nil)
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
	out := make([]Source, 0, len(resp.Web.Results))
	for _, r := range resp.Web.Results {
		out = append(out, Source{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return out, nil
}

// ── SearXNG（自托管） ──

// SearXNGProvider 通过自托管 SearXNG 搜索（JSON 格式；需在 SearXNG 里启用
// format=json）。
type SearXNGProvider struct {
	// BaseURL 是 SearXNG 实例地址（空 = 不可用）。
	BaseURL string
}

// NewSearXNG 构造 SearXNG 搜索后端。
func NewSearXNG(baseURL string) *SearXNGProvider { return &SearXNGProvider{BaseURL: baseURL} }

// Name 实现 Provider。
func (p *SearXNGProvider) Name() string { return "searxng" }

// Search 实现 Provider。
func (p *SearXNGProvider) Search(ctx context.Context, query string, limit int) ([]Source, error) {
	if p.BaseURL == "" {
		return nil, fmt.Errorf("searxng: 缺少 base URL")
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	req, err := newRequest(ctx, http.MethodGet,
		strings.TrimRight(p.BaseURL, "/")+"/search?q="+url.QueryEscape(query)+"&format=json", nil)
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
	out := make([]Source, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, Source{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var (
	_ Provider = (*DuckDuckGoProvider)(nil)
	_ Provider = (*TavilyProvider)(nil)
	_ Provider = (*SerperProvider)(nil)
	_ Provider = (*BraveProvider)(nil)
	_ Provider = (*SearXNGProvider)(nil)
)
