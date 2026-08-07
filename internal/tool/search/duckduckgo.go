package search

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// duckduckgoDefaultEndpoint 是 DuckDuckGo HTML 版搜索地址（免费，无需 API Key）。
const duckduckgoDefaultEndpoint = "https://html.duckduckgo.com/html/"

// DuckDuckGoProvider 通过 DuckDuckGo HTML 结果页实现搜索，免费且无需 API Key。
// 可选 Region（如 cn-cn）与 SafeSearch（off | moderate | strict）。
type DuckDuckGoProvider struct {
	Region     string // 区域参数 kl，如 cn-cn；空使用默认
	SafeSearch string // off | moderate | strict；空为 moderate
	Endpoint   string // 自定义端点；空使用 html.duckduckgo.com
}

func (p *DuckDuckGoProvider) Name() string { return "duckduckgo" }

func (p *DuckDuckGoProvider) Search(ctx context.Context, query string, numResults int) ([]Result, error) {
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
	resp, err := http.DefaultClient.Do(req)
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
	return parseDuckDuckGo(body, numResults), nil
}

// ddgSafeSearchKP 将安全级别映射为 DDG 的 kp 参数；未知值返回空（使用默认）。
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

// parseDuckDuckGo 从结果页解析标题/链接/摘要，最多返回 max 条。
func parseDuckDuckGo(body []byte, max int) []Result {
	s := string(body)
	titles := ddgTitleRe.FindAllStringSubmatch(s, -1)
	snips := ddgSnipRe.FindAllStringSubmatch(s, -1)
	n := len(titles)
	if n > len(snips) {
		n = len(snips)
	}
	if n > max {
		n = max
	}
	out := make([]Result, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Result{
			Title:   stripHTML([]byte(titles[i][2])),
			URL:     ddgCleanURL(titles[i][1]),
			Snippet: stripHTML([]byte(snips[i][1])),
		})
	}
	return out
}

// ddgCleanURL 将 DuckDuckGo 跳转链接还原为目标地址；非跳转链接原样返回。
func ddgCleanURL(href string) string {
	href = html.UnescapeString(href)
	if strings.HasPrefix(href, ddgURLPrefix) {
		if u, err := url.Parse(href); err == nil {
			if d := u.Query().Get("uddg"); d != "" {
				return d
			}
		}
	}
	return strings.TrimPrefix(href, "//")
}

var (
	stripTagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	stripScriptRe  = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(?:script|style|noscript)>`)
	stripEntityRe  = regexp.MustCompile(`&(amp|lt|gt|quot|apos|nbsp|#\d+);`)
	stripNumEntity = regexp.MustCompile(`&#(\d+);`)
)

// stripHTML 将 HTML 片段转换为可读纯文本（去 script/style/标签并解码实体）。
func stripHTML(b []byte) string {
	s := string(b)
	s = stripScriptRe.ReplaceAllString(s, " ")
	s = stripTagRe.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&apos;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = stripNumEntity.ReplaceAllStringFunc(s, func(e string) string {
		if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(e, "&#"), ";")); err == nil {
			return string(rune(n))
		}
		return e
	})
	return strings.Join(strings.Fields(s), " ")
}
