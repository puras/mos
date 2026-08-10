package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDuckDuckGoSearch(t *testing.T) {
	ddg := `<html><body>
		<div class="result">
			<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&amp;rut=abc">Go <b>Programming</b> Language</a>
			<a class="result__snippet" href="//duckduckgo.com/l/?uddg=x">Official site &amp; documentation</a>
		</div>
	</body></html>`
	var q, kl, kp string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, kl, kp = r.URL.Query().Get("q"), r.URL.Query().Get("kl"), r.URL.Query().Get("kp")
		w.Write([]byte(ddg))
	}))
	defer srv.Close()

	p := &DuckDuckGoProvider{Region: "cn-cn", SafeSearch: "strict", Endpoint: srv.URL}
	res, err := p.Search(context.Background(), "golang", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if q != "golang" || kl != "cn-cn" || kp != "-2" {
		t.Fatalf("params q=%q kl=%q kp=%q", q, kl, kp)
	}
	if len(res) != 1 {
		t.Fatalf("results = %d, want 1", len(res))
	}
	if res[0].Title != "Go Programming Language" || res[0].URL != "https://go.dev/" || res[0].Snippet != "Official site & documentation" {
		t.Fatalf("result = %+v", res[0])
	}
}

func TestDuckDuckGoSafeSearchDefault(t *testing.T) {
	if got := ddgSafeSearchKP(""); got != "1" {
		t.Fatalf("default kp = %q, want 1 (moderate)", got)
	}
	if got := ddgSafeSearchKP("off"); got != "-1" {
		t.Fatalf("off kp = %q", got)
	}
	if got := ddgSafeSearchKP("bogus"); got != "" {
		t.Fatalf("bogus kp = %q, want empty", got)
	}
}

func TestTavilySearch(t *testing.T) {
	var gotKey, gotQuery string
	var gotNum int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			APIKey     string `json:"api_key"`
			Query      string `json:"query"`
			MaxResults int    `json:"max_results"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotKey, gotQuery, gotNum = body.APIKey, body.Query, body.MaxResults
		w.Write([]byte(`{"results":[{"title":"T","url":"https://t.dev","content":"snippet"}]}`))
	}))
	defer srv.Close()

	tavilyEndpoint = srv.URL
	p := &TavilyProvider{APIKey: "k"}
	res, err := p.Search(context.Background(), "go", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotKey != "k" || gotQuery != "go" || gotNum != 3 {
		t.Fatalf("body key=%q query=%q num=%d", gotKey, gotQuery, gotNum)
	}
	if len(res) != 1 || res[0].Title != "T" || res[0].Snippet != "snippet" {
		t.Fatalf("results = %+v", res)
	}
}

func TestSerperSearch(t *testing.T) {
	var gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-KEY")
		var body struct {
			Q string `json:"q"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotQuery = body.Q
		w.Write([]byte(`{"organic":[{"title":"T","link":"https://t.dev","snippet":"s"}]}`))
	}))
	defer srv.Close()

	serperEndpoint = srv.URL
	p := &SerperProvider{APIKey: "k"}
	res, err := p.Search(context.Background(), "go", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotKey != "k" || gotQuery != "go" {
		t.Fatalf("key=%q query=%q", gotKey, gotQuery)
	}
	if len(res) != 1 || res[0].URL != "https://t.dev" {
		t.Fatalf("results = %+v", res)
	}
}

func TestBraveSearch(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query().Get("q")
		w.Write([]byte(`{"web":{"results":[{"title":"T","url":"https://t.dev","description":"d"}]}}`))
	}))
	defer srv.Close()

	braveEndpoint = srv.URL
	p := &BraveProvider{APIKey: "k"}
	res, err := p.Search(context.Background(), "go", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotAuth != "Token k" || gotQuery != "go" {
		t.Fatalf("auth=%q query=%q", gotAuth, gotQuery)
	}
	if len(res) != 1 || res[0].Snippet != "d" {
		t.Fatalf("results = %+v", res)
	}
}

func TestSearXNGFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no json", http.StatusForbidden)
	}))
	defer srv.Close()
	p := &SearXNGProvider{BaseURL: srv.URL}
	if _, err := p.Search(context.Background(), "go", 5); err == nil {
		t.Fatal("403 should fail")
	}
}

func TestNew(t *testing.T) {
	if _, err := New(Config{}); err != nil { // 空 → duckduckgo
		t.Fatalf("default: %v", err)
	}
	if _, err := New(Config{Provider: "duckduckgo"}); err != nil {
		t.Fatalf("duckduckgo: %v", err)
	}
	if _, err := New(Config{Provider: "tavily"}); err == nil {
		t.Fatal("tavily without key should fail")
	}
	if _, err := New(Config{Provider: "tavily", APIKey: "k"}); err != nil {
		t.Fatalf("tavily: %v", err)
	}
	if _, err := New(Config{Provider: "searxng"}); err == nil {
		t.Fatal("searxng without base_url should fail")
	}
	if _, err := New(Config{Provider: "searxng", BaseURL: "https://s.local"}); err != nil {
		t.Fatalf("searxng: %v", err)
	}
	if _, err := New(Config{Provider: "bogus"}); err == nil {
		t.Fatal("unknown provider should fail")
	}
}
