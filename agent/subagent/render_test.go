package subagent

import (
	"path/filepath"
	"strings"
	"testing"
)

// 清单渲染是纯函数：同一份清单渲染两次逐字节一致（prompt cache 靠这个）。
func TestRenderIsDeterministic(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "reviewer"), "name: reviewer\ndescription: 只读代码审查", "正文")
	writeAgent(t, filepath.Join(root, "deployer"), "name: deployer\ndescription: 部署流程", "正文")

	cat := Load(Options{Dirs: []string{root}})
	first := cat.Render()
	second := cat.Render()
	if first != second {
		t.Fatalf("两次渲染不一致：\n%q\n%q", first, second)
	}
	if !strings.Contains(first, "<"+CatalogTag+">") || !strings.Contains(first, "</"+CatalogTag+">") {
		t.Fatalf("清单应带标签：\n%s", first)
	}
	if !strings.Contains(first, "- reviewer: 只读代码审查") {
		t.Fatalf("清单应含名字与描述：\n%s", first)
	}
}

// 没有子代理 → 清单为空（不留空洞）。
func TestRenderEmptyWhenNoSubagent(t *testing.T) {
	cat := Load(Options{Dirs: []string{t.TempDir()}})
	if got := cat.Render(); got != "" {
		t.Fatalf("没有子代理时清单应为空：\n%s", got)
	}
}

// 清单里带白名单（模型据此知道这个角色能用什么）。
func TestRenderShowsToolWhitelist(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "reviewer"), "name: reviewer\ndescription: 只读代码审查\ntools: [read, grep]", "正文")

	cat := Load(Options{Dirs: []string{root}})
	text := cat.Render()
	if !strings.Contains(text, "可用工具：read、grep") {
		t.Fatalf("清单应带白名单：\n%s", text)
	}
}

// 清单超预算：降级并如实说明（先丢描述，再丢条目），不静默丢。
func TestRenderDegradesUnderBudget(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"alpha", "beta", "gamma"} {
		writeAgent(t, filepath.Join(root, n), "name: "+n+"\ndescription: "+strings.Repeat("很长的一段描述", 6), "正文")
	}
	const budget = 300
	cat := Load(Options{Dirs: []string{root}, MaxCatalogBytes: budget})

	full := renderCatalog(cat.All(), true, 0)
	text := cat.Render()
	if len(text) > budget {
		t.Fatalf("渲染超出预算（%d 字节）：\n%s", len(text), text)
	}
	if len(full) <= budget {
		t.Fatalf("本用例前提失效：全量渲染 %d 字节 ≤ 预算", len(full))
	}
	if !strings.Contains(text, "预算受限") && !strings.Contains(text, "未列出") {
		t.Fatalf("降级应如实说明：\n%s", text)
	}
}

// 预算小到连一条都列不下 → 退到最小形态（仍不超预算）。
func TestRenderFallsBackToMinimal(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "alpha"), "name: alpha\ndescription: "+strings.Repeat("描述", 50), "正文")
	cat := Load(Options{Dirs: []string{root}, MaxCatalogBytes: 40})

	text := cat.Render()
	if len(text) > 40 {
		t.Fatalf("应退到最小形态：%q", text)
	}
	if text != "" && !strings.Contains(text, "未列出") {
		t.Fatalf("最小形态应说明没列：%q", text)
	}
}

// 预算太小（连标签都放不下）→ 不注入清单，但工具仍在。
func TestRenderEmptyWhenBudgetTooSmall(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "alpha"), "name: alpha\ndescription: 描述", "正文")
	cat := Load(Options{Dirs: []string{root}, MaxCatalogBytes: 4})
	if got := cat.Render(); got != "" {
		t.Fatalf("预算装不下标签时应返回空：%q", got)
	}
}
