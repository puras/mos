package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hegq.net/mos/tool"
)

const (
	webFetchMaxBytes = 2 << 20 // 最大响应体 2MB
	webFetchMaxRunes = 12000   // 返回给模型的文本上限
	webFetchTimeout  = 15 * time.Second
)

// WebFetch 抓取网页内容并转换为纯文本。
type WebFetch struct{}

type fetchParams struct {
	URL string `json:"url" mos:"desc=网页地址（http/https）"`
}

func (WebFetch) Name() string { return "web_fetch" }
func (WebFetch) Description() string {
	return "抓取指定网页的内容并转为文本，用于获取实时信息"
}
func (WebFetch) Parameters() any { return &fetchParams{} }
func (WebFetch) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[fetchParams](args)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
		return nil, fmt.Errorf("仅支持 http/https 地址")
	}

	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MoS-Agent/0.1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("抓取失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("抓取失败: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBytes))
	if err != nil {
		return nil, err
	}
	text := htmlToText(body)
	if len(text) > webFetchMaxRunes {
		text = text[:webFetchMaxRunes] + "...(截断)"
	}
	return map[string]any{
		"url":     p.URL,
		"title":   extractTitle(body),
		"content": text,
	}, nil
}

// extractTitle 提取 <title> 标签内容。
func extractTitle(body []byte) string {
	re := regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	m := re.FindSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

var (
	tagRe    = regexp.MustCompile(`(?s)<[^>]+>`)
	scriptRe = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(?:script|style|noscript)>`)
	entityRe = regexp.MustCompile(`&(amp|lt|gt|quot|apos|nbsp|#\d+);`)
)

// htmlToText 将 HTML 转换为可读纯文本。
func htmlToText(body []byte) string {
	s := string(body)
	s = scriptRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = entityRe.ReplaceAllStringFunc(s, func(e string) string {
		switch e {
		case "&amp;":
			return "&"
		case "&lt;":
			return "<"
		case "&gt;":
			return ">"
		case "&quot;":
			return `"`
		case "&apos;":
			return "'"
		case "&nbsp;":
			return " "
		default: // &#NN; 数字实体
			if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(e, "&#"), ";")); err == nil {
				return string(rune(n))
			}
			return e
		}
	})
	return strings.Join(strings.Fields(s), " ")
}

var _ tool.Tool = WebFetch{}
