package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── SSRF 防御 ──

// staticResolver 是固定地址的解析器（测试用）。
type staticResolver struct {
	ips []net.IP
	err error
}

func (s staticResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.ips, nil
}

// 内网地址一律拒绝（环回 / 私有段 / 链路本地 / 元数据地址）。
func TestFetchRejectsPrivateAddresses(t *testing.T) {
	cases := []string{"127.0.0.1", "10.0.0.5", "172.16.0.1", "192.168.1.1", "169.254.169.254", "::1"}
	for _, addr := range cases {
		f := HTTPFetcher{Resolver: staticResolver{ips: []net.IP{net.ParseIP(addr)}}}
		_, err := f.Fetch(context.Background(), "http://internal.example/")
		if err == nil {
			t.Fatalf("%s 应被拒绝", addr)
		}
		if !strings.Contains(err.Error(), "公网") {
			t.Fatalf("%s 的拒绝理由应说明不是公网地址: %v", addr, err)
		}
	}
}

// 公网地址判定：各种保留段都要挡住。
func TestIsPublicIP(t *testing.T) {
	private := []string{
		"0.0.0.0", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "192.0.0.1", "192.0.2.1", "192.168.0.1", "198.18.0.1",
		"198.51.100.1", "203.0.113.1", "224.0.0.1", "255.255.255.255",
		"::", "::1", "fc00::1", "fe80::1", "ff02::1",
		"::ffff:127.0.0.1", "64:ff9b::7f00:1", "2002:7f00:1::", "2001::1:2:3:4",
	}
	for _, ip := range private {
		if isPublicIP(net.ParseIP(ip)) {
			t.Fatalf("%s 不该被判为公网地址", ip)
		}
	}
	public := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"}
	for _, ip := range public {
		if !isPublicIP(net.ParseIP(ip)) {
			t.Fatalf("%s 应被判为公网地址", ip)
		}
	}
}

// URL 校验：只允许 http/https、拒绝内嵌凭据、拒绝超长 URL。
func TestValidateURL(t *testing.T) {
	if _, err := validateURL("ftp://x.dev/a"); err == nil {
		t.Fatal("ftp 应被拒绝")
	}
	if _, err := validateURL("file:///etc/passwd"); err == nil {
		t.Fatal("file 应被拒绝")
	}
	if _, err := validateURL("https://user:pass@x.dev/"); err == nil {
		t.Fatal("内嵌凭据应被拒绝")
	}
	if _, err := validateURL("https://x.dev/" + strings.Repeat("a", maxURLLength)); err == nil {
		t.Fatal("超长 URL 应被拒绝")
	}
	if _, err := validateURL("https://x.dev/ok"); err != nil {
		t.Fatalf("正常 URL 不该被拒: %v", err)
	}
}

// 跨源重定向拒绝（要求模型针对新地址重新调用）。
func TestFetchRefusesCrossOriginRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("other"))
	}))
	defer other.Close()
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/target", http.StatusFound)
	}))
	defer main.Close()

	f := testFetcher(main.URL)
	_, err := f.Fetch(context.Background(), main.URL+"/start")
	if err == nil {
		t.Fatal("跨源重定向应被拒绝")
	}
	if !strings.Contains(err.Error(), "跨源") {
		t.Fatalf("拒绝理由应说明跨源: %v", err)
	}
}

// 同源重定向允许（并跟到最终地址）。
func TestFetchFollowsSameOriginRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("到达终点"))
	}))
	defer srv.Close()

	f := testFetcher(srv.URL)
	res, err := f.Fetch(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatalf("同源重定向应被跟随: %v", err)
	}
	if !strings.Contains(res.Content, "到达终点") {
		t.Fatalf("内容不对: %q", res.Content)
	}
	if !strings.HasSuffix(res.URL, "/final") {
		t.Fatalf("最终 URL 应是重定向后的: %q", res.URL)
	}
}

// 二进制/未知内容类型 → 报错（不把二进制塞进模型上下文）。
func TestFetchRejectsUnsupportedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0x00, 0x01, 0x02})
	}))
	defer srv.Close()

	f := testFetcher(srv.URL)
	_, err := f.Fetch(context.Background(), srv.URL+"/bin")
	if err == nil {
		t.Fatal("不支持的内容类型应报错")
	}
	if !strings.Contains(err.Error(), "内容类型") {
		t.Fatalf("拒绝理由应说明内容类型: %v", err)
	}
}

// 体积上限：超量截断并标 truncated。
func TestFetchCapsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer srv.Close()

	f := HTTPFetcher{
		Resolver: staticResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}},
		Dial:     dialTo(srv.Listener.Addr().String()),
		Limits:   FetchLimits{MaxResponseBytes: 100, MaxContentChars: 100000},
	}
	res, err := f.Fetch(context.Background(), srv.URL+"/big")
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	if !res.Truncated {
		t.Fatal("超量时 truncated 应为 true")
	}
	if len(res.Content) > 100 {
		t.Fatalf("内容应被截到上限: %d 字节", len(res.Content))
	}
}

// HTML → 纯文本：去脚本、去标签、解实体；标题单独取。
func TestFetchConvertsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>页面标题</title><style>b{}</style></head>` +
			`<body><script>alert(1)</script><p>第一段</p><p>第二段 &amp; 更多</p></body></html>`))
	}))
	defer srv.Close()

	f := testFetcher(srv.URL)
	res, err := f.Fetch(context.Background(), srv.URL+"/p")
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	if res.Title != "页面标题" {
		t.Fatalf("title = %q", res.Title)
	}
	if strings.Contains(res.Content, "alert(1)") {
		t.Fatalf("脚本应被去掉: %q", res.Content)
	}
	if !strings.Contains(res.Content, "第一段") || !strings.Contains(res.Content, "& 更多") {
		t.Fatalf("正文不对: %q", res.Content)
	}
}

// 非 2xx 是**结果**不是错误（对齐 DSH 语义）。
func TestFetchNon2xxIsResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("没有这个页面"))
	}))
	defer srv.Close()

	f := testFetcher(srv.URL)
	res, err := f.Fetch(context.Background(), srv.URL+"/missing")
	if err != nil {
		t.Fatalf("404 不该是错误: %v", err)
	}
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status_code = %d, want 404", res.StatusCode)
	}
}

// testFetcher 造一个“能连本地测试服务、但地址判定为公网”的抓取器：
// 解析器给公网 IP，拨号器指向本地监听地址（生产不这么干——生产用钉住拨号）。
func testFetcher(baseURL string) HTTPFetcher {
	return HTTPFetcher{
		Resolver: staticResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}},
		Dial:     dialTo(localAddr(baseURL)),
		Limits:   FetchLimits{Timeout: 5 * time.Second},
	}
}

// dialTo 返回一个固定拨号器（连到指定地址）。
func dialTo(addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
	}
}

// localAddr 从 URL 里取出 host:port。
func localAddr(rawURL string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(rawURL, "http://"), "https://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}
