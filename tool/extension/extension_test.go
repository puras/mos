package extension

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/puras/mos/tool/extension/search"
)

func TestWebSearchExecute(t *testing.T) {
	ddg := `<html><body>
		<div class="result">
			<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&amp;rut=abc">Go <b>Programming</b> Language</a>
			<a class="result__snippet" href="//duckduckgo.com/l/?uddg=x">Official site &amp; documentation</a>
		</div>
		<div class="result">
			<a rel="nofollow" class="result__a" href="https://example.org">Example Org</a>
			<a class="result__snippet" href="#">Second result snippet</a>
		</div>
	</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(ddg))
	}))
	defer srv.Close()

	ws := WebSearch{Provider: &search.DuckDuckGoProvider{Endpoint: srv.URL}}
	res, err := ws.Execute(context.Background(), json.RawMessage(`{"query":"golang"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m := res.(map[string]any)
	results, ok := m["results"].([]search.Result)
	if !ok || len(results) != 2 {
		t.Fatalf("results = %#v", m["results"])
	}
	// 第一条：标题去 <b>、DDG 跳转链接还原、实体解码
	first := results[0]
	if first.Title != "Go Programming Language" {
		t.Fatalf("title = %q", first.Title)
	}
	if first.URL != "https://go.dev/" {
		t.Fatalf("url = %q", first.URL)
	}
	if first.Snippet != "Official site & documentation" {
		t.Fatalf("snippet = %q", first.Snippet)
	}
	// 第二条：非跳转链接原样返回（去掉 // 前缀）
	if results[1].URL != "https://example.org" {
		t.Fatalf("url2 = %q", results[1].URL)
	}
	// max_results 限制
	res2, err := ws.Execute(context.Background(), json.RawMessage(`{"query":"golang","max_results":1}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if n := len(res2.(map[string]any)["results"].([]search.Result)); n != 1 {
		t.Fatalf("max_results: got %d results", n)
	}
	// 空查询应报错
	if _, err := (WebSearch{}).Execute(context.Background(), json.RawMessage(`{"query":""}`)); err == nil {
		t.Fatal("empty query should fail")
	}
}
