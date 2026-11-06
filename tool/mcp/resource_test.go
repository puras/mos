package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/puras/mos/tool"
)

// 资源工具（S8.4-E）的守护测试。
//
// 覆盖拍板 ② 的验收口径：三个共享工具 + 显式 server 参数、hidden 服务器
// 资源不泄露、无资源服务器不注册、分页收集（fake 侧单页）、读取投影
// （文本直返 / 二进制 OpaquePart）、未知 server、读取失败。

// newResRegistry 造一个带资源工具的注册表（两台可见 + 一台 hidden）。
func newResRegistry(t *testing.T) (*tool.Registry, *Manager, map[string]*fakeClient) {
	t.Helper()
	reg := tool.NewRegistry()
	clients := map[string]*fakeClient{
		"docs": {
			resources: []ResourceInfo{
				{URI: "file:///b.txt", Name: "b"},
				{URI: "file:///a.txt", Name: "a", Description: "a 文件", MIMEType: "text/plain", Size: 12},
			},
			readContents: []ResourceContents{{MIMEType: "text/plain", Text: "正文"}},
		},
		"tpl": {
			templates: []ResourceTemplateInfo{{URITemplate: "file://logs/{date}.txt", Name: "日志", MIMEType: "text/plain"}},
		},
		"secret": {
			resources: []ResourceInfo{{URI: "file:///key.pem", Name: "私钥"}},
		},
		"plain": {}, // 没有资源
	}
	m := NewManager(Options{
		Servers: []ServerConfig{
			{Name: "docs"},
			{Name: "tpl"},
			{Name: "secret", Exposure: ExposureHidden},
			{Name: "plain"},
		},
		Dial: func(_ context.Context, cfg ServerConfig, _ Options) (Client, error) {
			return clients[cfg.Name], nil
		},
	}, reg)
	m.Start(context.Background())
	if err := m.RegisterResourceTools(); err != nil {
		t.Fatalf("注册资源工具: %v", err)
	}
	return reg, m, clients
}

func TestResourceToolsRegistered(t *testing.T) {
	reg, _, _ := newResRegistry(t)
	for _, name := range []string{ResourceListName, ResourceTemplName, ResourceReadName} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("资源工具 %s 应已注册：%v", name, reg.Names())
		}
	}
	// 资源工具是共享直出工具：直接进模型清单（不是 deferred）。
	for _, tl := range reg.List() {
		if tl.Name == ResourceListName || tl.Name == ResourceTemplName || tl.Name == ResourceReadName {
			if tl.Parameters == nil {
				t.Fatalf("资源工具 %s 应带参数 schema", tl.Name)
			}
		}
	}
}

func TestResourceToolsNotRegisteredWithoutResources(t *testing.T) {
	reg := tool.NewRegistry()
	m := NewManager(Options{
		Servers: []ServerConfig{{Name: "plain"}},
		Dial: func(context.Context, ServerConfig, Options) (Client, error) {
			return &fakeClient{}, nil
		},
	}, reg)
	m.Start(context.Background())
	if err := m.RegisterResourceTools(); err != nil {
		t.Fatalf("无资源时注册应无操作，得 %v", err)
	}
	for _, name := range []string{ResourceListName, ResourceTemplName, ResourceReadName} {
		if _, ok := reg.Get(name); ok {
			t.Fatalf("没有任何服务器有资源时不该注册 %s", name)
		}
	}
}

func TestResourceListServerParam(t *testing.T) {
	reg, _, _ := newResRegistry(t)
	tl, _ := reg.Get(ResourceListName)
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"server":"docs"}`))
	if err != nil {
		t.Fatalf("列出 docs: %v", err)
	}
	out := res.(map[string]any)
	if out["server"] != "docs" {
		t.Fatalf("server 应回显，得 %v", out["server"])
	}
	list := out["resources"].([]map[string]any)
	if len(list) != 2 {
		t.Fatalf("docs 应有 2 个资源，得 %d", len(list))
	}
	// 确定性顺序：按 URI 排序。
	if list[0]["uri"] != "file:///a.txt" || list[1]["uri"] != "file:///b.txt" {
		t.Fatalf("资源应按 URI 排序，得 %v / %v", list[0]["uri"], list[1]["uri"])
	}
	if list[0]["mimeType"] != "text/plain" || list[0]["size"] != int64(12) {
		t.Fatalf("投影缺字段：%v", list[0])
	}
}

func TestResourceListUnknownServer(t *testing.T) {
	reg, _, _ := newResRegistry(t)
	tl, _ := reg.Get(ResourceListName)
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"server":"nope"}`)); err == nil {
		t.Fatal("未知服务器应报错")
	}
	// hidden 服务器的资源不泄露：与未知服务器同待遇（模型拿不到服务器名）。
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"server":"secret"}`)); err == nil {
		t.Fatal("hidden 服务器的资源不该可达")
	}
	// 没有资源的服务器也不可达（注册门与调用时同口径）。
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"server":"plain"}`)); err == nil {
		t.Fatal("无资源服务器不该可达")
	}
}

func TestResourceTemplates(t *testing.T) {
	reg, _, _ := newResRegistry(t)
	tl, _ := reg.Get(ResourceTemplName)
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"server":"tpl"}`))
	if err != nil {
		t.Fatalf("列出 tpl 模板: %v", err)
	}
	tmpls := res.(map[string]any)["templates"].([]map[string]any)
	if len(tmpls) != 1 || tmpls[0]["uriTemplate"] != "file://logs/{date}.txt" {
		t.Fatalf("模板投影不符：%v", tmpls)
	}
}

func TestResourceReadText(t *testing.T) {
	reg, _, _ := newResRegistry(t)
	tl, _ := reg.Get(ResourceReadName)
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"server":"docs","uri":"file:///a.txt"}`))
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	out := res.(map[string]any)
	if out["content"] != "正文" {
		t.Fatalf("文本应直返，得 %v", out["content"])
	}
	if _, has := out["opaque"]; has {
		t.Fatalf("文本资源不该有 opaque：%v", out)
	}
}

func TestResourceReadBinaryGoesOpaque(t *testing.T) {
	reg := tool.NewRegistry()
	blob := []byte("binary")
	m := NewManager(Options{
		Servers: []ServerConfig{{Name: "bin"}},
		Dial: func(context.Context, ServerConfig, Options) (Client, error) {
			return &fakeClient{
				resources:    []ResourceInfo{{URI: "file:///img.png"}},
				readContents: []ResourceContents{{MIMEType: "image/png", Blob: blob}},
			}, nil
		},
	}, reg)
	m.Start(context.Background())
	if err := m.RegisterResourceTools(); err != nil {
		t.Fatalf("注册: %v", err)
	}
	tl, _ := reg.Get(ResourceReadName)
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"server":"bin","uri":"file:///img.png"}`))
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	out := res.(map[string]any)
	if _, has := out["content"]; has {
		t.Fatalf("二进制不该进 content：%v", out["content"])
	}
	opaqueJSON, err := json.Marshal(out["opaque"])
	if err != nil {
		t.Fatalf("opaque 序列化: %v", err)
	}
	var parts []OpaquePart
	if err := json.Unmarshal(opaqueJSON, &parts); err != nil {
		t.Fatalf("opaque 反序列化: %v", err)
	}
	if len(parts) != 1 || parts[0].Type != "resource" {
		t.Fatalf("二进制应降级成 resource 占位：%v", parts)
	}
	if !strings.Contains(parts[0].Note, "6 字节") {
		t.Fatalf("占位说明应带字节数：%s", parts[0].Note)
	}
}

func TestResourceReadFailure(t *testing.T) {
	reg := tool.NewRegistry()
	m := NewManager(Options{
		Servers: []ServerConfig{{Name: "docs"}},
		Dial: func(context.Context, ServerConfig, Options) (Client, error) {
			return &fakeClient{
				resources: []ResourceInfo{{URI: "file:///a.txt"}},
				readErr:   &Error{Kind: ErrorProtocol, Server: "docs", Err: errors.New("读不了")},
			}, nil
		},
	}, reg)
	m.Start(context.Background())
	_ = m.RegisterResourceTools()
	tl, _ := reg.Get(ResourceReadName)
	if _, err := tl.Execute(context.Background(), json.RawMessage(`{"server":"docs","uri":"file:///a.txt"}`)); err == nil {
		t.Fatal("读取失败应报错")
	}
}

func TestResourceToolsSchemaRender(t *testing.T) {
	// 三个工具的参数 schema 应能正常反射（server 必填进 required）。
	reg, _, _ := newResRegistry(t)
	var raw []byte
	for _, tl := range reg.List() {
		if tl.Name == ResourceReadName {
			raw, _ = json.Marshal(tl.Parameters)
		}
	}
	if raw == nil {
		t.Fatalf("read 工具应进模型清单：%v", reg.Names())
	}
	s := string(raw)
	if !strings.Contains(s, `"server"`) || !strings.Contains(s, `"uri"`) {
		t.Fatalf("schema 应含 server 与 uri 参数：%s", s)
	}
	if !strings.Contains(s, `"required"`) {
		t.Fatalf("server/uri 应必填：%s", s)
	}
}

func TestRegisterResourceToolsIdempotent(t *testing.T) {
	_, m, _ := newResRegistry(t)
	if err := m.RegisterResourceTools(); err != nil {
		t.Fatalf("重复注册应幂等，得 %v", err)
	}
}
