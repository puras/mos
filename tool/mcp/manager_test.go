package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/puras/mos/tool"
)

// dialerStub 按服务器名返回假客户端（"bad" 那台连不上）。
func dialerStub(clients map[string]*fakeClient) func(context.Context, ServerConfig, Options) (Client, error) {
	return func(_ context.Context, cfg ServerConfig, _ Options) (Client, error) {
		if cfg.Name == "bad" {
			return nil, errors.New("连不上")
		}
		c, ok := clients[cfg.Name]
		if !ok {
			return nil, errors.New("没有这台服务器")
		}
		return c, nil
	}
}

func TestManagerFailureIsolation(t *testing.T) {
	good := &fakeClient{tools: []ToolInfo{{Name: "a", Description: "A"}}}
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{Name: "bad"}, {Name: "ok"}},
		Dial:    dialerStub(map[string]*fakeClient{"ok": good}),
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()

	// 好那台照常注册。
	if _, ok := reg.Get("mcp__ok__a"); !ok {
		t.Fatalf("好服务器的工具应注册：%v", reg.Names())
	}
	// 坏那台只留一条警告。
	warns := mgr.Warnings()
	if len(warns) != 1 {
		t.Fatalf("应有一条降级诊断：%v", warns)
	}
	if len(mgr.Servers()) != 1 || mgr.Servers()[0].Name() != "ok" {
		t.Fatalf("只应有 1 台连上的服务器：%v", mgr.Servers())
	}
}

func TestManagerExposure(t *testing.T) {
	client := &fakeClient{tools: []ToolInfo{
		{Name: "d", Description: "deferred 工具"},
		{Name: "r", Description: "direct 工具"},
		{Name: "h", Description: "hidden 工具"},
	}}
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{
			Name:     "srv",
			Exposure: ExposureDeferred,
			ToolExposure: map[string]Exposure{
				"r": ExposureDirect,
				"h": ExposureHidden,
			},
		}},
		Dial: dialerStub(map[string]*fakeClient{"srv": client}),
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()

	// hidden 档靠可见性层挡：注册一个过滤器模拟装配层的接法。
	reg.AddVisibility(mgr.HiddenFilter())

	// 模型清单：只有 direct 那个（deferred 不在其中，hidden 被挡）。
	names := map[string]bool{}
	for _, tl := range reg.List() {
		names[tl.Name] = true
	}
	if !names["mcp__srv__r"] {
		t.Fatalf("direct 工具该在清单里：%v", names)
	}
	if names["mcp__srv__d"] {
		t.Fatalf("deferred 工具不该在清单里：%v", names)
	}
	if names["mcp__srv__h"] {
		t.Fatalf("hidden 工具不该在清单里：%v", names)
	}
	// deferred 工具能被搜到（tool_search 的入口），hidden 不能。
	deferred := map[string]bool{}
	for _, d := range reg.DeferredTools() {
		deferred[d.Name] = true
	}
	if !deferred["mcp__srv__d"] {
		t.Fatalf("deferred 工具应能被搜到：%v", deferred)
	}
	if deferred["mcp__srv__h"] {
		t.Fatalf("hidden 工具不该被搜到：%v", deferred)
	}
	// 隐藏 ≠ 禁止调用。
	if _, ok := reg.Get("mcp__srv__h"); !ok {
		t.Fatal("hidden 工具应仍可被程序化调用")
	}
}

func TestManagerRefreshKeepsPreviousGenerationOnFailure(t *testing.T) {
	client := &fakeClient{tools: []ToolInfo{{Name: "a", Description: "A"}}}
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{Name: "srv"}},
		Dial:    dialerStub(map[string]*fakeClient{"srv": client}),
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()

	// 换代成功：旧工具消失，新工具进注册表。
	client.setTools([]ToolInfo{{Name: "b", Description: "B"}})
	mgr.refreshServer("srv")
	if _, ok := reg.Get("mcp__srv__a"); ok {
		t.Fatalf("旧工具应消失：%v", reg.Names())
	}
	if _, ok := reg.Get("mcp__srv__b"); !ok {
		t.Fatalf("新工具应进注册表：%v", reg.Names())
	}

	// 换代失败：保留上一代，不留半套。
	client.mu.Lock()
	client.listErr = errors.New("服务器失联")
	client.mu.Unlock()
	mgr.refreshServer("srv")
	if _, ok := reg.Get("mcp__srv__b"); !ok {
		t.Fatalf("刷新失败应保留上一代：%v", reg.Names())
	}
	if len(mgr.Warnings()) == 0 {
		t.Fatal("刷新失败应留一条诊断")
	}
}

func TestManagerHiddenFollowsGeneration(t *testing.T) {
	client := &fakeClient{tools: []ToolInfo{{Name: "h", Description: "H"}}}
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{
			Name: "srv",
			ToolExposure: map[string]Exposure{
				"h":  ExposureHidden,
				"h2": ExposureHidden,
			},
		}},
		Dial: dialerStub(map[string]*fakeClient{"srv": client}),
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()
	reg.AddVisibility(mgr.HiddenFilter())

	// 过滤器返回 true = **可见**：hidden 工具应返回 false。
	if mgr.HiddenFilter()("mcp__srv__h") {
		t.Fatal("hidden 工具应被过滤器挡住")
	}
	// 换代后工具改名 → hidden 集合跟着换（老名字不再挡，新名字继续挡）。
	client.setTools([]ToolInfo{{Name: "h2", Description: "H2"}})
	mgr.refreshServer("srv")
	if !mgr.HiddenFilter()("mcp__srv__h") {
		t.Fatal("换代后老名字不该还在 hidden 集合里")
	}
	if mgr.HiddenFilter()("mcp__srv__h2") {
		t.Fatal("新名字该进 hidden 集合")
	}
}

func TestManagerInstructions(t *testing.T) {
	client := &fakeClient{instructions: "这台服务器能做 XYZ", protocol: "2026-07-28"}
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{Name: "srv"}},
		Dial:    dialerStub(map[string]*fakeClient{"srv": client}),
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()

	ins := mgr.Instructions()
	if len(ins) != 1 || ins[0].Server != "srv" || ins[0].Text != "这台服务器能做 XYZ" {
		t.Fatalf("服务器说明应带归属：%v", ins)
	}
	if mgr.Servers()[0].ProtocolVersion() != "2026-07-28" {
		t.Fatalf("协议版本应记在快照里：%q", mgr.Servers()[0].ProtocolVersion())
	}
}

// 换代信号：内部换代与宿主回调**两件都要发生**（不能互相顶掉）。
func TestManagerOnToolsChangedComposesHostCallback(t *testing.T) {
	client := &fakeClient{tools: []ToolInfo{{Name: "a", Description: "A"}}}
	reg := tool.NewRegistry()

	var hostCalls []string
	var signal func(string)
	mgr := NewManager(Options{
		Servers: []ServerConfig{{Name: "srv"}},
		Dial: func(_ context.Context, cfg ServerConfig, opts Options) (Client, error) {
			signal = opts.OnToolsChanged
			return client, nil
		},
		OnToolsChanged: func(server string) {
			hostCalls = append(hostCalls, server)
		},
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()

	if signal == nil {
		t.Fatal("Dial 应拿到换代信号回调")
	}
	// 服务器换代：工具集变了。
	client.setTools([]ToolInfo{{Name: "b", Description: "B"}})
	signal("srv")

	// 内部换代异步发生（换代在别的 goroutine 里做）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Get("mcp__srv__b"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := reg.Get("mcp__srv__b"); !ok {
		t.Fatalf("换代信号应触发内部换代：%v", reg.Names())
	}
	// 宿主的回调也要被调到（不能被内部信号覆盖）。
	if len(hostCalls) != 1 || hostCalls[0] != "srv" {
		t.Fatalf("宿主回调应被调用一次：%v", hostCalls)
	}
}

// 故意禁用的服务器：不算故障，但快照要写明「没连」。
func TestManagerSkipsDisabledServers(t *testing.T) {
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{Name: "off", Disabled: true}},
		Dial:    dialerStub(map[string]*fakeClient{}),
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()

	if len(mgr.Servers()) != 0 {
		t.Fatalf("禁用服务器不该连接：%v", mgr.Servers())
	}
	skipped := mgr.Skipped()
	if len(skipped) != 1 || skipped[0] != "off" {
		t.Fatalf("禁用服务器应记在未连接名单里：%v", skipped)
	}
	// 禁用不是故障 → 不进降级诊断。
	if len(mgr.Warnings()) != 0 {
		t.Fatalf("禁用不该记成降级诊断：%v", mgr.Warnings())
	}
}
