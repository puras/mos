package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HTTPFetcher 是基于 net/http 的 Fetcher 实现（默认实现）。
//
// 它是本包安全边界最重的一块——抓的是**敌意输入**，且 URL 由模型决定：
//
//  1. **URL 校验**：长度上限、只允许 http/https、禁止 URL 内嵌凭据；
//  2. **公网地址校验**：解析主机名，**所有**地址都必须是公网地址——私有段、
//     环回、链路本地（含 169.254.169.254 元数据）、ULA、组播、CGNAT、
//     保留段一律拒绝；IPv6 里的内嵌 IPv4（IPv4-mapped / NAT64 / 6to4 /
//     Teredo）也按 IPv4 判；
//  3. **地址钉住**（防 DNS rebinding）：校验过的地址集直接交给传输层拨号，
//     **不再二次解析**——否则"检查时是公网、连接时变内网"就能穿透；
//  4. **重定向只跟同源**：跨源重定向拒绝（要求模型针对新 URL 重新调用，
//     每个新源都要重新过一遍校验）；
//  5. **体积/时间上限**：响应体字节上限、字符上限、超时。
//
// 诚实边界：
//   - **不走 httpx 注入的客户端**——钉住传输需要自己控制 DialContext，
//     宿主的代理/自定义传输在这里用不上。需要代理的宿主请自备 Fetcher
//     实现（实现 Fetcher 接口即可）。
//   - **只解 UTF-8**：不识别页面声明的非 UTF-8 字符集（不引第三方依赖）——
//     乱码比报错更糟，但本包也不假装自己处理了编码。
//   - **不判"内容是否恶意"**：只做地址与体积的硬边界；内容层的"这是资料不是
//     指令"靠 ExternalContentNotice 标注（模型侧的事）。
type HTTPFetcher struct {
	// Limits 是资源上限（零值取默认）。
	Limits FetchLimits
	// Resolver 是 DNS 解析器（测试可替换；nil = 系统解析）。
	Resolver Resolver
	// Dial 是拨号器（测试可替换；nil = 钉住拨号）。
	//
	// 给了它就**不再钉住地址**——调用方自负“连到哪”的责任（自建代理、
	// 测试内网服务时才需要）。生产默认 nil：钉住是防 DNS rebinding 的关键。
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// FetchLimits 是抓取的���源上限。
type FetchLimits struct {
	// MaxResponseBytes 是响应体字节上限（默认 5MB）。
	MaxResponseBytes int64
	// MaxContentChars 是转成纯文本后的字符上限（默认 100K）。
	MaxContentChars int
	// Timeout 是单次抓取的超时（默认 30s）。
	Timeout time.Duration
	// MaxRedirects 是允许跟随的同源重定向跳数（默认 5）。
	MaxRedirects int
	// UserAgent 是请求头（默认显式产品标识——**不伪装浏览器**）。
	UserAgent string
}

// 抓取默认值（对位 DSH 的 web-fetch-http 默认值）。
const (
	defaultMaxResponseBytes = 5 << 20 // 5MB
	defaultMaxContentChars  = 100000
	defaultFetchTimeout     = 30 * time.Second
	defaultMaxRedirects     = 5
	defaultFetchUserAgent   = "mos-web-fetch/1.0 (+https://github.com/puras/mos)"
	// maxURLLength 是 URL 长度上限（对齐 DSH 的 WEB_FETCH_MAX_URL_LENGTH）。
	maxURLLength = 2048
)

// Resolver 是 DNS 解析（可替换，便于测试）。
type Resolver interface {
	// LookupIP 返回主机名的全部地址（IPv4 + IPv6）。
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

// systemResolver 是系统 DNS 解析。
type systemResolver struct{}

// LookupIP 实现 Resolver。
func (systemResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// Name 实现 Fetcher。
func (HTTPFetcher) Name() string { return "http" }

// Fetch 实现 Fetcher。
func (f HTTPFetcher) Fetch(ctx context.Context, rawURL string) (FetchResult, error) {
	limits := f.limits()
	u, err := validateURL(rawURL)
	if err != nil {
		return FetchResult{}, err
	}

	// 主机名 → 地址集（必须全是公网地址）。
	host := u.Hostname()
	if host == "" {
		return FetchResult{}, fmt.Errorf("web: URL 缺少主机名: %q", rawURL)
	}
	resolver := f.Resolver
	if resolver == nil {
		resolver = systemResolver{}
	}
	ips, err := resolver.LookupIP(ctx, host)
	if err != nil {
		return FetchResult{}, fmt.Errorf("web: 解析主机 %q 失败: %w", host, err)
	}
	if len(ips) == 0 {
		return FetchResult{}, fmt.Errorf("web: 主机 %q 没有可用地址", host)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return FetchResult{}, fmt.Errorf("web: 拒绝抓取内网地址 %q（%s 不是公网地址）", host, ip)
		}
	}

	// 钉住传输：拨号只走上面校验过的地址，不再解析主机名。
	dial := f.Dial
	if dial == nil {
		dial = pinnedDialer(host, ips)
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil, // 代理会自己做名字解析 → 破坏钉住
			DialContext:           dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: limits.Timeout,
			DisableKeepAlives:     true,
		},
		Timeout: limits.Timeout,
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= limits.MaxRedirects {
			return fmt.Errorf("web: 重定向跳数超过上限 %d", limits.MaxRedirects)
		}
		if len(via) == 0 {
			return nil
		}
		prev := via[len(via)-1].URL
		if !sameOrigin(prev, req.URL) {
			return fmt.Errorf("web: 拒绝跨源重定向 %s → %s（请针对新地址重新调用）", prev, req.URL)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("User-Agent", limits.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("web: 抓取失败: %w", err)
	}
	defer resp.Body.Close()

	// 体积上限：多读一个字节用来判断"被截断了"。
	body, err := io.ReadAll(io.LimitReader(resp.Body, limits.MaxResponseBytes+1))
	if err != nil {
		return FetchResult{}, fmt.Errorf("web: 读取响应体失败: %w", err)
	}
	truncated := false
	if int64(len(body)) > limits.MaxResponseBytes {
		body = body[:limits.MaxResponseBytes]
		truncated = true
	}

	kind, err := classifyContentType(resp.Header.Get("Content-Type"))
	if err != nil {
		return FetchResult{}, err
	}
	content := string(body)
	title := ""
	if kind == "html" {
		title = extractTitle(content)
		content = htmlToText(content)
	}
	if max := limits.MaxContentChars; len([]rune(content)) > max {
		content = string([]rune(content)[:max])
		truncated = true
	}
	return FetchResult{
		URL:         resp.Request.URL.String(),
		StatusCode:  resp.StatusCode,
		Title:       title,
		ContentType: resp.Header.Get("Content-Type"),
		Content:     content,
		Truncated:   truncated,
	}, nil
}

// limits 归一化资源上限。
func (f HTTPFetcher) limits() FetchLimits {
	l := f.Limits
	if l.MaxResponseBytes <= 0 {
		l.MaxResponseBytes = defaultMaxResponseBytes
	}
	if l.MaxContentChars <= 0 {
		l.MaxContentChars = defaultMaxContentChars
	}
	if l.Timeout <= 0 {
		l.Timeout = defaultFetchTimeout
	}
	if l.MaxRedirects <= 0 {
		l.MaxRedirects = defaultMaxRedirects
	}
	if l.UserAgent == "" {
		l.UserAgent = defaultFetchUserAgent
	}
	return l
}

// validateURL 校验 URL 的**与网络无关**的部分：长度、协议、内嵌凭据。
func validateURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("web: url 不能为空")
	}
	if len(raw) > maxURLLength {
		return nil, fmt.Errorf("web: URL 超过最大长度 %d", maxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("web: URL 无效: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("web: 只支持 http/https，收到 %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("web: URL 里不允许内嵌凭据")
	}
	return u, nil
}

// sameOrigin 判定两个 URL 是否同源（协议 + 主机 + 端口）。
func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		portOf(a) == portOf(b)
}

// portOf 取 URL 的有效端口（未显式给出时按协议取默认）。
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// pinnedDialer 造一个只拨**已校验地址**的拨号器。
//
// 防的是 DNS rebinding：检查时解析到公网 IP，连接时再解析一次可能得到内网
// 地址。这里把地址钉死，并且拒绝连接未校验的主机（理论上被重定向骗到）。
func pinnedDialer(host string, ips []net.IP) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		h, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(h, host) {
			return nil, fmt.Errorf("web: 拒绝连接未校验的主机 %q", h)
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, fmt.Errorf("web: 连接 %q 失败: %w", host, lastErr)
	}
}

// ── 公网地址判定 ──

// isPublicIP 报告地址是否公网可路由。
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		return isPublicIPv4(v4)
	}
	return isPublicIPv6(ip.To16())
}

// isPublicIPv4 判定 IPv4 是否公网地址。
func isPublicIPv4(v4 net.IP) bool {
	switch {
	case v4[0] == 0: // 0.0.0.0/8
		return false
	case v4[0] == 10: // 10/8
		return false
	case v4[0] == 127: // 127/8 环回
		return false
	case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64/10 CGNAT
		return false
	case v4[0] == 169 && v4[1] == 254: // 169.254/16 链路本地（含云元数据 169.254.169.254）
		return false
	case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31: // 172.16/12
		return false
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0/24（协议保留，含 NAT64 发现地址）
		return false
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 2: // 192.0.2/24 TEST-NET
		return false
	case v4[0] == 192 && v4[1] == 168: // 192.168/16
		return false
	case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18/15 基准测试
		return false
	case v4[0] == 198 && v4[1] == 51 && v4[2] == 100: // 198.51.100/24 TEST-NET
		return false
	case v4[0] == 203 && v4[1] == 0 && v4[2] == 113: // 203.0.113/24 TEST-NET
		return false
	case v4[0] >= 224: // 224/4 组播 + 240/4 保留 + 广播
		return false
	}
	return true
}

// isPublicIPv6 判定 IPv6 是否公网地址（含内嵌 IPv4 的各种变体）。
func isPublicIPv6(ip net.IP) bool {
	v6 := ip.To16()
	if v6 == nil {
		return false
	}
	if v6.IsUnspecified() || v6.IsLoopback() || v6.IsPrivate() ||
		v6.IsLinkLocalUnicast() || v6.IsLinkLocalMulticast() ||
		v6.IsInterfaceLocalMulticast() || v6.IsMulticast() {
		return false
	}
	// IPv4-mapped：::ffff:0:0/96（To4 已处理，这里兜底防手工构造）
	if hasPrefix(v6, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff) {
		return isPublicIPv4(embeddedIPv4(v6, 96))
	}
	// NAT64 熟知前缀 64:ff9b::/96
	if hasPrefix(v6, 0x00, 0x64, 0xff, 0x9b, 0, 0, 0, 0, 0, 0, 0, 0) {
		return isPublicIPv4(embeddedIPv4(v6, 96))
	}
	// NAT64 本地使用前缀 64:ff9b:1::/48
	if hasPrefix(v6, 0x00, 0x64, 0xff, 0x9b, 0x00, 0x01) {
		return isPublicIPv4(embeddedIPv4(v6, 48))
	}
	// 6to4：2002::/16，内嵌 IPv4 在第 2–5 字节
	if v6[0] == 0x20 && v6[1] == 0x02 {
		return isPublicIPv4(net.IPv4(v6[2], v6[3], v6[4], v6[5]))
	}
	// Teredo：2001::/32，内嵌 IPv4 在第 12–15 字节（异或 0xFF 混淆）
	if v6[0] == 0x20 && v6[1] == 0x01 && v6[2] == 0x00 && v6[3] == 0x00 {
		return isPublicIPv4(net.IPv4(^v6[12], ^v6[13], ^v6[14], ^v6[15]))
	}
	return true
}

// embeddedIPv4 提取 IPv6 中紧随 prefixLen 位前缀之后的 4 字节（IPv4 形式）。
func embeddedIPv4(v6 net.IP, prefixLen int) net.IP {
	if prefixLen%8 != 0 {
		return nil
	}
	off := prefixLen / 8
	if off+4 > len(v6) {
		return nil
	}
	return net.IPv4(v6[off], v6[off+1], v6[off+2], v6[off+3])
}

// hasPrefix 判定 IPv6 地址是否以给定字节开头。
func hasPrefix(v6 net.IP, prefix ...byte) bool {
	if len(v6) < len(prefix) {
		return false
	}
	for i, b := range prefix {
		if v6[i] != b {
			return false
		}
	}
	return true
}

// ── 内容处理 ──

// classifyContentType 把 Content-Type 归成可解的种类；不支持的类型报错
// （对位 DSH：宁可响亮失败，也不要把二进制当文本塞进模型上下文）。
func classifyContentType(contentType string) (string, error) {
	mime := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch {
	case mime == "":
		return "", fmt.Errorf("web: 响应没有 Content-Type，无法判定是否为文本")
	case mime == "text/html", mime == "application/xhtml+xml":
		return "html", nil
	case strings.HasPrefix(mime, "text/"):
		return "text", nil
	case mime == "application/json", mime == "application/xml",
		strings.HasSuffix(mime, "+json"), strings.HasSuffix(mime, "+xml"):
		return "text", nil
	default:
		return "", fmt.Errorf("web: 不支持的内容类型 %q（只取文本类）", mime)
	}
}

var (
	htmlTitleRe  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	htmlScriptRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg)[^>]*>.*?</(?:script|style|noscript|svg)>`)
	htmlTagRe    = regexp.MustCompile(`(?s)<[^>]+>`)
	htmlBreakRe  = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/tr|/h[1-6]|/section|/article|/header|/footer|/blockquote|pre)[^>]*>`)
	htmlNumEnt   = regexp.MustCompile(`&#(\d+);`)
	htmlHexEnt   = regexp.MustCompile(`&#[xX]([0-9a-fA-F]+);`)
)

// extractTitle 取 <title> 文本。
func extractTitle(html string) string {
	m := htmlTitleRe.FindStringSubmatch(html)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(htmlToText(m[1]))
}

// htmlToText 把 HTML 转成可读纯文本：去脚本与样式、块级标签换行、去标签、
// 解实体、压空白。
func htmlToText(html string) string {
	s := htmlScriptRe.ReplaceAllString(html, " ")
	s = htmlBreakRe.ReplaceAllString(s, "\n")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = decodeEntities(s)
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.Join(strings.Fields(l), " ")
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// decodeEntities 解常见的 HTML 实体（数值实体一并解）。
func decodeEntities(s string) string {
	repl := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&apos;", "'",
		"&nbsp;", " ",
		"&hellip;", "…",
		"&mdash;", "—",
		"&ndash;", "–",
	)
	s = repl.Replace(s)
	s = htmlNumEnt.ReplaceAllStringFunc(s, func(e string) string {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(e, "&#"), ";"))
		if err != nil || n <= 0 || n > 0x10FFFF {
			return e
		}
		return string(rune(n))
	})
	s = htmlHexEnt.ReplaceAllStringFunc(s, func(e string) string {
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(e, "&"), "#"), ";"), 16, 32)
		if err != nil || n <= 0 || n > 0x10FFFF {
			return e
		}
		return string(rune(n))
	})
	return s
}
