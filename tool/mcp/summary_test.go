package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/puras/mos/tool"
)

// 只有 deferred 工具的服务器才进摘要节（direct 模型看得见、hidden 够不着）。
func TestSummariesOnlyDeferredServers(t *testing.T) {
	client := &fakeClient{tools: []ToolInfo{
		{Name: "d", Description: "deferred"},
		{Name: "r", Description: "direct"},
		{Name: "h", Description: "hidden"},
	}}
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{
			Name:         "srv",
			Description:  "一句话摘要\n第二行不要",
			ToolExposure: map[string]Exposure{"r": ExposureDirect, "h": ExposureHidden},
		}},
		Dial: dialerStub(map[string]*fakeClient{"srv": client}),
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()

	sums := mgr.Summaries()
	if len(sums) != 1 {
		t.Fatalf("应只有 1 台服务器进摘要节：%+v", sums)
	}
	if sums[0].Namespace != "mcp__srv" {
		t.Fatalf("命名空间不对：%q", sums[0].Namespace)
	}
	// 摘要只取第一行。
	if sums[0].Description != "一句话摘要" {
		t.Fatalf("摘要应只取首行：%q", sums[0].Description)
	}
}

// 全是 direct 的服务器 → 不进摘要节（模型已经看得见，列了是浪费）。
func TestSummariesSkipDirectOnlyServers(t *testing.T) {
	client := &fakeClient{tools: []ToolInfo{{Name: "r", Description: "direct"}}}
	reg := tool.NewRegistry()
	mgr := NewManager(Options{
		Servers: []ServerConfig{{
			Name:         "srv",
			ToolExposure: map[string]Exposure{"r": ExposureDirect},
		}},
		Dial: dialerStub(map[string]*fakeClient{"srv": client}),
	}, reg)
	mgr.Start(context.Background())
	defer mgr.Close()

	if sums := mgr.Summaries(); len(sums) != 0 {
		t.Fatalf("全是 direct 的服务器不该进摘要节：%+v", sums)
	}
}

// 渲染：首行讲怎么够到，每行一个服务器，摘要在冒号后。
func TestRenderServersSection(t *testing.T) {
	out := RenderServersSection([]Summary{
		{Server: "gh", Namespace: "mcp__gh", Description: "GitHub 工单"},
		{Server: "fs", Namespace: "mcp__fs", Description: "本地文件"},
	}, 0)
	if !strings.Contains(out, "tool_search") {
		t.Fatalf("首行应说明怎么够到：\n%s", out)
	}
	if !strings.Contains(out, "- mcp__gh (tool_search): GitHub 工单") {
		t.Fatalf("服务器行格式不对：\n%s", out)
	}
	if !strings.Contains(out, "- mcp__fs (tool_search): 本地文件") {
		t.Fatalf("服务器行格式不对：\n%s", out)
	}
	// 按名字排序（确定性输出 → prompt cache 稳定）。
	if strings.Index(out, "mcp__fs") > strings.Index(out, "mcp__gh") {
		t.Fatalf("服务器应按名字排序：\n%s", out)
	}
}

// 空列表 → 空串（不加分节，不留空洞）。
func TestRenderServersSectionEmpty(t *testing.T) {
	if out := RenderServersSection(nil, 0); out != "" {
		t.Fatalf("没有服务器应返回空串：%q", out)
	}
}

// 预算行为（不变式风格：文案变了也不会假失败）：
//   - 输出不超预算；
//   - 放不下的服务器被砍掉时，末尾写明还剩几个；
//   - 预算连一行都放不下 → 整节不渲染（留空壳更糟）。
func TestRenderServersSectionBudget(t *testing.T) {
	subs := []Summary{
		{Server: "a", Namespace: "mcp__a", Description: strings.Repeat("描", 200)},
		{Server: "b", Namespace: "mcp__b", Description: strings.Repeat("述", 200)},
		{Server: "c", Namespace: "mcp__c", Description: strings.Repeat("很", 200)},
	}
	for _, budget := range []int{1, 20, 60, 100, 144, 200, 512, 4096} {
		out := RenderServersSection(subs, budget)
		if out == "" {
			continue // 预算太小 → 整节不渲染（合法）
		}
		if len(out) > budget {
			t.Fatalf("预算 %d：渲染结果超预算（%d 字节）：\n%s", budget, len(out), out)
		}
		listed := strings.Count(out, "(tool_search)")
		if listed > len(subs) {
			t.Fatalf("预算 %d：列出的服务器数超过总数：%d", budget, listed)
		}
		if listed < len(subs) {
			if !strings.Contains(out, "另有") {
				t.Fatalf("预算 %d：砍掉了服务器却没写明还剩几个：\n%s", budget, out)
			}
		}
	}
	// 默认预算：三个都列得出，描述被截断并带省略号。
	full := RenderServersSection(subs, 0)
	if strings.Count(full, "(tool_search)") != 3 {
		t.Fatalf("默认预算应列全部服务器：\n%s", full)
	}
	if !strings.Contains(full, "…") {
		t.Fatalf("超长描述应被截断：\n%s", full)
	}
}

// 预算够时：全部列出 + 摘要按剩余预算均分（描述被截断要带省略号）。
func TestRenderServersSectionTruncatesDescription(t *testing.T) {
	long := strings.Repeat("x", 600)
	out := RenderServersSection([]Summary{
		{Server: "a", Namespace: "mcp__a", Description: long},
	}, 0)
	if !strings.Contains(out, "…") {
		t.Fatalf("超长摘要应被截断并带省略号：\n%s", out)
	}
	if len(out) > DefaultMaxServersSectionBytes {
		t.Fatalf("超预算：%d 字节", len(out))
	}
}

// 截断不切坏 UTF-8（按 rune 退到边界）。
func TestTruncateBytesKeepsUTF8(t *testing.T) {
	s := strings.Repeat("技", 100) // 每个 3 字节
	out := truncateBytes(s, 50)
	if len(out) > 50 {
		t.Fatalf("截断后仍超限：%d", len(out))
	}
	for _, r := range out {
		if r == '�' {
			t.Fatalf("截断切坏了 UTF-8：%q", out)
		}
	}
}

// 首行截取：描述是「一句话」。
func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"a\nb":  "a",
		"a":     "a",
		"  a  ": "a",
		"":      "",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Fatalf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
