package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/puras/mos/tool"
)

// fakeClient 是 Client 的测试替身。
type fakeClient struct {
	mu           sync.Mutex
	tools        []ToolInfo
	instructions string
	protocol     string
	listErr      error
	callErr      error
	result       *Result
	calls        []string
}

func (f *fakeClient) ListTools(context.Context) ([]ToolInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]ToolInfo(nil), f.tools...), nil
}

func (f *fakeClient) CallTool(_ context.Context, name string, _ map[string]any) (*Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if f.callErr != nil {
		return nil, f.callErr
	}
	if f.result != nil {
		return f.result, nil
	}
	return &Result{Content: "ok:" + name}, nil
}

func (f *fakeClient) Instructions() string { return f.instructions }
func (f *fakeClient) ProtocolVersion() string {
	return f.protocol
}
func (f *fakeClient) Close() error { return nil }

// setTools 换掉工具集（模拟服务器换代）。
func (f *fakeClient) setTools(tools []ToolInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools = tools
}

func TestToolAdapter(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "title": "文本"},
		},
		"required": []any{"text"},
	}
	client := &fakeClient{result: &Result{Content: "回显"}}
	tl := NewTool("srv", ToolInfo{Name: "echo", Description: "回显", InputSchema: schema}, client)

	if tl.Name() != "mcp__srv__echo" {
		t.Fatalf("工具名 = %q", tl.Name())
	}
	if tl.Description() != "回显" {
		t.Fatalf("描述 = %q", tl.Description())
	}
	// schema 直通：注册后清单里的 schema 应与上游一致（不反推）。
	reg := tool.NewRegistry()
	if err := reg.Register(tl); err != nil {
		t.Fatalf("Register: %v", err)
	}
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("清单应有 1 个工具：%v", list)
	}
	got, _ := json.Marshal(list[0].Parameters)
	want, _ := json.Marshal(schema)
	if string(got) != string(want) {
		t.Fatalf("schema 没原样透传：\n got %s\nwant %s", got, want)
	}

	// 执行：参数透传，返回值是投影结果。
	out, err := tl.Execute(context.Background(), json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	res, ok := out.(*Result)
	if !ok || res.Content != "回显" {
		t.Fatalf("返回值应是 *Result：%v", out)
	}
}

func TestToolAdapterUpstreamError(t *testing.T) {
	client := &fakeClient{result: &Result{IsError: true, Content: "参数不合法"}}
	tl := NewTool("srv", ToolInfo{Name: "echo"}, client)
	_, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("上游 isError 应变成 Go error（模型看得见错误文本、可自纠）")
	}
	if !strings.Contains(err.Error(), "参数不合法") {
		t.Fatalf("错误文本应带上游内容：%v", err)
	}
	var mcpErr *Error
	if !errors.As(err, &mcpErr) || mcpErr.Kind != ErrorProtocol {
		t.Fatalf("应归类为协议错误：%v", err)
	}
}

func TestToolAdapterBadArgs(t *testing.T) {
	client := &fakeClient{}
	tl := NewTool("srv", ToolInfo{Name: "echo"}, client)
	if _, err := tl.Execute(context.Background(), json.RawMessage(`[1,2]`)); err == nil {
		t.Fatal("参数不是 JSON 对象应报错")
	}
}
