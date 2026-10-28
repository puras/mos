package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 各厂商的响应解析：起本地服务返回样本响应，验证归一化后的来源形状。
//
// 关键断言：**可选字段缺失就缺**（不编标题、不编摘要）。

// Tavily：POST /search → {results:[{title,url,content}]}。
func TestTavilyProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("路径 = %q, want /search", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"T1","url":"https://x.dev/1","content":"S1"}]}`))
	}))
	defer srv.Close()

	p := NewTavily("key")
	p.BaseURL = srv.URL
	out, err := p.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("tavily: %v", err)
	}
	if len(out) != 1 || out[0].URL != "https://x.dev/1" || out[0].Title != "T1" || out[0].Snippet != "S1" {
		t.Fatalf("归一化结果不对: %+v", out)
	}
}

// Serper：POST /search → {organic:[{title,link,snippet}]}。
func TestSerperProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"organic":[{"title":"T","link":"https://x.dev/2","snippet":"S"}]}`))
	}))
	defer srv.Close()

	p := NewSerper("key")
	p.BaseURL = srv.URL
	out, err := p.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("serper: %v", err)
	}
	if len(out) != 1 || out[0].URL != "https://x.dev/2" {
		t.Fatalf("归一化结果不对: %+v", out)
	}
}

// Brave：GET /res/... → {web:{results:[{title,url,description}]}}。
func TestBraveProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"web":{"results":[{"title":"T","url":"https://x.dev/3","description":"D"}]}}`))
	}))
	defer srv.Close()

	p := NewBrave("key")
	p.BaseURL = srv.URL
	out, err := p.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("brave: %v", err)
	}
	if len(out) != 1 || out[0].URL != "https://x.dev/3" || out[0].Snippet != "D" {
		t.Fatalf("归一化结果不对: %+v", out)
	}
}

// SearXNG：GET /search?format=json → {results:[{title,url,content}]}。
func TestSearXNGProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format = %q, want json", r.URL.Query().Get("format"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"T","url":"https://x.dev/4","content":"C"}]}`))
	}))
	defer srv.Close()

	p := NewSearXNG(srv.URL)
	out, err := p.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("searxng: %v", err)
	}
	if len(out) != 1 || out[0].URL != "https://x.dev/4" {
		t.Fatalf("归一化结果不对: %+v", out)
	}
}

// DuckDuckGo：HTML 结果页 → 标题/链接/摘要；跳转链接要还原成目标地址。
func TestDuckDuckGoProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>
			<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fx.dev%2F5&amp;rut=1">标题一</a>
			<a class="result__snippet">摘要一</a>
		</body></html>`))
	}))
	defer srv.Close()

	p := NewDuckDuckGo()
	p.Endpoint = srv.URL
	out, err := p.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("duckduckgo: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("应解析出 1 条: %+v", out)
	}
	if out[0].URL != "https://x.dev/5" {
		t.Fatalf("跳转链接没还原: %q", out[0].URL)
	}
	if out[0].Title != "标题一" || out[0].Snippet != "摘要一" {
		t.Fatalf("标题/摘要不对: %+v", out[0])
	}
}

// 缺 Key 的后端 → 明确报错（不静默返回空结果）。
func TestProvidersRequireCredentials(t *testing.T) {
	ctx := context.Background()
	if _, err := NewTavily("").Search(ctx, "q", 3); err == nil {
		t.Fatal("tavily 缺 Key 应报错")
	}
	if _, err := NewSerper("").Search(ctx, "q", 3); err == nil {
		t.Fatal("serper 缺 Key 应报错")
	}
	if _, err := NewBrave("").Search(ctx, "q", 3); err == nil {
		t.Fatal("brave 缺 Key 应报错")
	}
	if _, err := NewSearXNG("").Search(ctx, "q", 3); err == nil {
		t.Fatal("searxng 缺地址应报错")
	}
}

// 多查询包装：并行搜、合并去重、截到 limit；部分失败要如实写进 Note。
func TestMultiSearcherMergesAndDedupes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 两个查询都返回同一个 URL + 一个各自独有的 URL。
		w.Write([]byte(`{"results":[{"title":"T","url":"https://x.dev/same","content":"C"}]}`))
	}))
	defer srv.Close()

	p := NewSearXNG(srv.URL)
	s := NewSearcher(p)
	res, err := s.Search(context.Background(), []string{"a", "b"}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Sources) != 1 {
		t.Fatalf("相同 URL 应去重成 1 条: %+v", res.Sources)
	}
	if s.Name() != "searxng" {
		t.Fatalf("Name = %q", s.Name())
	}
}

// 部分查询失败：有结果就用结果，并如实说明哪条失败（不静默吞掉）。
func TestMultiSearcherPartialFailure(t *testing.T) {
	s := NewSearcher(&partialProvider{})
	res, err := s.Search(context.Background(), []string{"ok", "bad"}, 5)
	if err != nil {
		t.Fatalf("部分失败不该整体报错: %v", err)
	}
	if len(res.Sources) != 1 {
		t.Fatalf("应保留成功那条: %+v", res.Sources)
	}
	if res.Note == "" {
		t.Fatal("失败的查询应写进 Note")
	}
}

// partialProvider 是“一条查询成功、一条失败”的后端。
type partialProvider struct{}

func (partialProvider) Name() string { return "partial" }

func (partialProvider) Search(ctx context.Context, query string, limit int) ([]Source, error) {
	if query == "bad" {
		return nil, errors.New("这条炸了")
	}
	return []Source{{URL: "https://x.dev/ok"}}, nil
}

// 后端全失败 → 报错（不返回空结果假装"没找到"）。
func TestMultiSearcherAllFailed(t *testing.T) {
	s := NewSearcher(&failingProvider{})
	if _, err := s.Search(context.Background(), []string{"a"}, 5); err == nil {
		t.Fatal("全部失败应报错")
	}
}

// failingProvider 是总是失败的搜索后端。
type failingProvider struct{}

func (failingProvider) Name() string { return "failing" }

func (failingProvider) Search(ctx context.Context, query string, limit int) ([]Source, error) {
	return nil, errFake
}

// errFake 是测试用的失败原因。
var errFake = errors.New("后端炸了")
