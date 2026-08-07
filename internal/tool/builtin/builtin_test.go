package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"hegq.net/mos/internal/tool/search"
	"hegq.net/mos/pkg/tool"
)

func TestEvalExpr(t *testing.T) {
	cases := []struct {
		expr string
		want float64
	}{
		{"1+2", 3},
		{"1+2*3", 7},
		{"(1+2)*3", 9},
		{"10/4", 2.5},
		{"-5+3", -2},
		{"7%3", 1},
		{" 2 * (3 + 4) ", 14},
	}
	for _, c := range cases {
		got, err := evalExpr(c.expr)
		if err != nil {
			t.Fatalf("eval %q: %v", c.expr, err)
		}
		if got != c.want {
			t.Fatalf("eval %q = %v, want %v", c.expr, got, c.want)
		}
	}
	if _, err := evalExpr("1+"); err == nil {
		t.Fatal("invalid expr should fail")
	}
	if _, err := evalExpr("1/0"); err == nil {
		t.Fatal("divide by zero should fail")
	}
}

func TestCalculatorExecute(t *testing.T) {
	res, err := Calculator{}.Execute(context.Background(), json.RawMessage(`{"expr":"(1+2)*3"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res != "9" {
		t.Fatalf("result = %v, want 9", res)
	}
}

func TestCurrentTimeExecute(t *testing.T) {
	res, err := CurrentTime{}.Execute(context.Background(), json.RawMessage(`{"timezone":"UTC"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m := res.(map[string]any)
	if m["timezone"] != "UTC" {
		t.Fatalf("timezone = %v", m["timezone"])
	}
	if _, ok := m["time"].(string); !ok {
		t.Fatalf("time missing: %+v", m)
	}
	// 无效时区应报错
	if _, err := (CurrentTime{}).Execute(context.Background(), json.RawMessage(`{"timezone":"Mars/Olympus"}`)); err == nil {
		t.Fatal("invalid timezone should fail")
	}
}

func TestWebFetchExecute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io := `<html><head><title>测试页</title><script>var x=1;</script></head>
			<body><h1>你好</h1><p>世界&nbsp;!</p></body></html>`
		w.Write([]byte(io))
	}))
	defer srv.Close()

	res, err := WebFetch{}.Execute(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m := res.(map[string]any)
	if m["title"] != "测试页" {
		t.Fatalf("title = %v", m["title"])
	}
	content, _ := m["content"].(string)
	if !strings.Contains(content, "你好") || !strings.Contains(content, "世界 !") {
		t.Fatalf("content = %q", content)
	}
	if strings.Contains(content, "script") {
		t.Fatalf("script should be stripped: %q", content)
	}
	// 非 http 地址应拒绝
	if _, err := (WebFetch{}).Execute(context.Background(), json.RawMessage(`{"url":"file:///etc/passwd"}`)); err == nil {
		t.Fatal("file url should fail")
	}
}

func TestShellExecute(t *testing.T) {
	res, err := Shell{}.Execute(context.Background(), json.RawMessage(`{"command":"echo hello-mos"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m := res.(map[string]any)
	if !strings.Contains(m["output"].(string), "hello-mos") {
		t.Fatalf("output = %v", m["output"])
	}
	if m["exit_code"].(int) != 0 {
		t.Fatalf("exit code = %v", m["exit_code"])
	}
}

// 沙箱路径（M1.6）：配置了执行沙箱时，Shell 在 WorkDir（jail）内执行。
func TestShellExecuteWithSandbox(t *testing.T) {
	dir := t.TempDir()
	ctx := tool.WithSandbox(context.Background(), &tool.Sandbox{WorkDir: dir})
	res, err := Shell{}.Execute(ctx, json.RawMessage(`{"command":"pwd"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m := res.(map[string]any)
	want, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(m["output"].(string), want) {
		t.Fatalf("output = %v, want contain %v", m["output"], want)
	}
}

func TestFileTools(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "sub", "a.txt")

	// 写入（自动建父目录）
	if _, err := (WriteFile{}).Execute(context.Background(), mustJSON(map[string]any{"path": fp, "content": "hello"})); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 读取
	res, err := ReadFile{}.Execute(context.Background(), mustJSON(map[string]any{"path": fp}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.(map[string]any)["content"] != "hello" {
		t.Fatalf("read content = %v", res)
	}
	// 搜索
	fres, err := FindFiles{}.Execute(context.Background(), mustJSON(map[string]any{"root": dir, "pattern": "*.txt"}))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(fres.(map[string]any)["matches"].([]string)) != 1 {
		t.Fatalf("find matches = %v", fres)
	}
	// 内容检索
	gres, err := Grep{}.Execute(context.Background(), mustJSON(map[string]any{"root": dir, "pattern": "hello"}))
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	gm := gres.(map[string]any)
	if gm["count"].(int) != 1 {
		t.Fatalf("grep count = %v", gm)
	}
}

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

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
