package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/puras/mos/tool"
)

// ── 假后端 ──

// fakeSearcher 是可控的搜索后端。
type fakeSearcher struct {
	queries [][]string // 收到的查询（每次调用一条）
	sources []Source
	err     error
}

func (f *fakeSearcher) Name() string { return "fake" }

func (f *fakeSearcher) Search(ctx context.Context, queries []string, limit int) (SearchResult, error) {
	for _, q := range queries {
		f.queries = append(f.queries, []string{q})
	}
	if f.err != nil {
		return SearchResult{}, f.err
	}
	srcs := f.sources
	truncated := false
	if limit > 0 && len(srcs) > limit {
		srcs = srcs[:limit]
		truncated = true
	}
	return SearchResult{Sources: srcs, Truncated: truncated}, nil
}

// fakeFetcher 是可控的抓取后端。
type fakeFetcher struct {
	url string
	res FetchResult
	err error
}

func (f *fakeFetcher) Name() string { return "fake" }

func (f *fakeFetcher) Fetch(ctx context.Context, u string) (FetchResult, error) {
	f.url = u
	if f.err != nil {
		return FetchResult{}, f.err
	}
	return f.res, nil
}

// ── web_search ──

// 工具名与 schema：名字是契约。
func TestSearchToolShape(t *testing.T) {
	s := NewSearch(&fakeSearcher{})
	if s.Name() != "web_search" {
		t.Fatalf("Name = %q, want web_search", s.Name())
	}
	schema := s.Parameters()
	if schema == nil {
		t.Fatal("Parameters 不该为 nil")
	}
	if !strings.Contains(s.Description(), "queries") {
		t.Fatalf("描述里应讲清 queries: %s", s.Description())
	}
}

// queries[] 与 query 两种形状都收；去重保序。
func TestSearchNormalizesQueries(t *testing.T) {
	fs := &fakeSearcher{sources: []Source{{URL: "https://x.dev/1"}}}
	s := NewSearch(fs)
	if _, err := s.Execute(context.Background(), []byte(`{"queries":["a"," b ","a"],"limit":3}`)); err != nil {
		t.Fatalf("web_search: %v", err)
	}
	if len(fs.queries) != 2 {
		t.Fatalf("去重后应剩 2 条查询, got %v", fs.queries)
	}
	if fs.queries[0][0] != "a" || fs.queries[1][0] != "b" {
		t.Fatalf("查询内容/顺序不对: %v", fs.queries)
	}

	// 单条形状（容错）。
	fs2 := &fakeSearcher{sources: []Source{{URL: "https://x.dev/1"}}}
	s2 := NewSearch(fs2)
	if _, err := s2.Execute(context.Background(), []byte(`{"query":"single"}`)); err != nil {
		t.Fatalf("web_search(query): %v", err)
	}
	if len(fs2.queries) != 1 || fs2.queries[0][0] != "single" {
		t.Fatalf("query 形状没被归一: %v", fs2.queries)
	}
}

// 空查询 → 报错；超量查询 → 报错（fail-closed，不静默截）。
func TestSearchRejectsBadQueries(t *testing.T) {
	s := NewSearch(&fakeSearcher{})
	if _, err := s.Execute(context.Background(), []byte(`{"queries":[]}`)); err == nil {
		t.Fatal("空查询应报错")
	}
	if _, err := s.Execute(context.Background(), []byte(`{"queries":["1","2","3","4","5"]}`)); err == nil {
		t.Fatal("超过 4 条查询应报错")
	}
}

// 未装配后端 → 明确报错（不给模型一个必然失败的工具）。
func TestSearchWithoutBackendFails(t *testing.T) {
	s := NewSearch(nil)
	if _, err := s.Execute(context.Background(), []byte(`{"queries":["x"]}`)); err == nil {
		t.Fatal("未装配后端应报错")
	} else if !strings.Contains(err.Error(), "未装配") {
		t.Fatalf("错误理由应说明未装配: %v", err)
	}
}

// limit 收口：后端多给的来源被截断，且如实标 truncated。
func TestSearchLimitTruncates(t *testing.T) {
	fs := &fakeSearcher{sources: []Source{
		{URL: "https://x.dev/1"}, {URL: "https://x.dev/2"}, {URL: "https://x.dev/3"},
	}}
	s := NewSearch(fs)
	out, err := s.Execute(context.Background(), []byte(`{"queries":["x"],"limit":2}`))
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}
	m := out.(map[string]any)
	if m["count"].(int) != 2 {
		t.Fatalf("count = %v, want 2", m["count"])
	}
	if !m["truncated"].(bool) {
		t.Fatal("超量时 truncated 应为 true")
	}
}

// 渲染文本带**不可信标注**（防提示注入的最便宜一道防线）。
func TestSearchRendersUntrustedNotice(t *testing.T) {
	fs := &fakeSearcher{sources: []Source{{URL: "https://x.dev/1", Title: "T", Snippet: "S"}}}
	s := NewSearch(fs)
	out, err := s.Execute(context.Background(), []byte(`{"queries":["x"]}`))
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}
	content := out.(map[string]any)["content"].(string)
	if !strings.Contains(content, ExternalContentNotice) {
		t.Fatalf("内容应带不可信标注: %s", content)
	}
	if !strings.Contains(content, "https://x.dev/1") {
		t.Fatalf("内容应含来源 URL: %s", content)
	}
}

// ── web_fetch ──

func TestFetchToolShape(t *testing.T) {
	f := NewFetch(&fakeFetcher{})
	if f.Name() != "web_fetch" {
		t.Fatalf("Name = %q, want web_fetch", f.Name())
	}
}

// 回执带 status_code / title / content，且 content 带不可信标注。
func TestFetchResultShape(t *testing.T) {
	ff := &fakeFetcher{res: FetchResult{
		URL: "https://x.dev/p", StatusCode: 200, Title: "标题", Content: "正文",
	}}
	f := NewFetch(ff)
	out, err := f.Execute(context.Background(), []byte(`{"url":"https://x.dev/p"}`))
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	m := out.(map[string]any)
	if m["status_code"].(int) != 200 {
		t.Fatalf("status_code = %v", m["status_code"])
	}
	if m["title"].(string) != "标题" {
		t.Fatalf("title = %v", m["title"])
	}
	if !strings.Contains(m["content"].(string), ExternalContentNotice) {
		t.Fatal("抓回内容应带不可信标注")
	}
	if ff.url != "https://x.dev/p" {
		t.Fatalf("URL 没传下去: %q", ff.url)
	}
}

// 空 URL → 报错；未装配后端 → 报错。
func TestFetchRejectsBadInput(t *testing.T) {
	f := NewFetch(&fakeFetcher{})
	if _, err := f.Execute(context.Background(), []byte(`{"url":""}`)); err == nil {
		t.Fatal("空 URL 应报错")
	}
	f2 := NewFetch(nil)
	if _, err := f2.Execute(context.Background(), []byte(`{"url":"https://x.dev"}`)); err == nil {
		t.Fatal("未装配后端应报错")
	}
}

// 内容超长 → 截断并标注（不让一次抓取撑爆上下文）。
func TestFetchTruncatesContent(t *testing.T) {
	ff := &fakeFetcher{res: FetchResult{URL: "https://x.dev/p", Content: strings.Repeat("字", 500)}}
	f := &Fetch{Fetcher: ff, MaxContentChars: 100}
	out, err := f.Execute(context.Background(), []byte(`{"url":"https://x.dev/p"}`))
	if err != nil {
		t.Fatalf("web_fetch: %v", err)
	}
	m := out.(map[string]any)
	if !m["truncated"].(bool) {
		t.Fatal("超长时 truncated 应为 true")
	}
	if !strings.Contains(m["content"].(string), "已截断") {
		t.Fatalf("内容应标注截断: %v", m["content"])
	}
}

// ── 装配 ──

// Tools：nil 后端 → 不给那个工具（能力即证据）。
func TestToolsSkipsMissingBackends(t *testing.T) {
	if got := Tools(nil, nil); len(got) != 0 {
		t.Fatalf("两个后端都没有时不应有工具, got %d", len(got))
	}
	if got := Tools(&fakeSearcher{}, nil); len(got) != 1 || got[0].Name() != "web_search" {
		t.Fatalf("只有搜索后端时应只给 web_search: %v", got)
	}
	if got := Tools(nil, &fakeFetcher{}); len(got) != 1 || got[0].Name() != "web_fetch" {
		t.Fatalf("只有抓取后端时应只给 web_fetch: %v", got)
	}
	if got := Tools(&fakeSearcher{}, &fakeFetcher{}); len(got) != 2 {
		t.Fatalf("两个后端都该给, got %d", len(got))
	}
}

// 工具的 JSON Schema 里参数名正确（模型看得见的形状）——经注册表生成。
func TestSearchSchemaParams(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(NewSearch(&fakeSearcher{})); err != nil {
		t.Fatal(err)
	}
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("注册表里应有 1 个工具, got %d", len(list))
	}
	b, err := json.Marshal(list[0].Parameters)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"queries", "query", "limit"} {
		if !strings.Contains(string(b), `"`+want+`"`) {
			t.Fatalf("schema 缺参数 %q: %s", want, b)
		}
	}
}
