package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
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

// Default 应包含且仅包含 7 个内置工具。
func TestDefault(t *testing.T) {
	names := map[string]bool{}
	for _, tl := range Default() {
		if names[tl.Name()] {
			t.Fatalf("duplicate tool %q in Default", tl.Name())
		}
		names[tl.Name()] = true
	}
	want := []string{"calculator", "current_time", "read_file", "write_file", "find_files", "grep", "web_fetch"}
	if len(names) != len(want) {
		t.Fatalf("Default() has %d tools, want %d", len(names), len(want))
	}
	for _, n := range want {
		if !names[n] {
			t.Fatalf("Default() missing %q", n)
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
