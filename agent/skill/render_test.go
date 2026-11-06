package skill

import (
	"path/filepath"
	"strings"
	"testing"
)

// 清单渲染是纯函数：同一份清单渲染两次逐字节一致（prompt cache 靠这个）。
func TestRenderIsDeterministic(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署流程\nwhen-to-use: 上线时", "步骤 1")
	writeSkill(t, filepath.Join(root, "review"), "name: review\ndescription: 代码评审", "步骤 2")

	cat := Load(Options{Dirs: []string{root}})
	first := cat.Render()
	second := cat.Render()
	if first != second {
		t.Fatalf("两次渲染不一致：\n%q\n%q", first, second)
	}
	if !strings.Contains(first, "<"+CatalogTag+">") || !strings.Contains(first, "</"+CatalogTag+">") {
		t.Fatalf("清单应带标签：\n%s", first)
	}
	if !strings.Contains(first, "- deploy: 部署流程") {
		t.Fatalf("清单应含名字与描述：\n%s", first)
	}
	if !strings.Contains(first, "（适用：上线时）") {
		t.Fatalf("清单应含适用场景：\n%s", first)
	}
}

// 没有模型可见技能 → 清单为空（不留空洞）。
func TestRenderEmptyWhenNoVisibleSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "user"), "name: user-only\ndescription: 仅用户\ndisable-model-invocation: true", "正文")

	cat := Load(Options{Dirs: []string{root}})
	if got := cat.Render(); got != "" {
		t.Fatalf("仅用户可调用的技能不该进清单：\n%s", got)
	}
}

// 清单超预算：降级并如实说明（先丢描述，再丢条目），不静默丢。
func TestRenderDegradesUnderBudget(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"alpha", "beta", "gamma"} {
		writeSkill(t, filepath.Join(root, n), "name: "+n+"\ndescription: "+strings.Repeat("很长的一段描述", 6), "正文")
	}
	const budget = 300
	cat := Load(Options{Dirs: []string{root}, MaxCatalogBytes: budget})

	full := renderCatalog(cat.ModelVisible(), true, 0)
	text := cat.Render()
	if len(text) > budget {
		t.Fatalf("渲染超出预算（%d 字节）：\n%s", len(text), text)
	}
	if len(full) <= budget {
		t.Fatalf("本用例前提失效：全量渲染 %d 字节 ≤ 预算", len(full))
	}
	if !strings.Contains(text, "预算受限") && !strings.Contains(text, "未列出") {
		t.Fatalf("超预算应如实说明降级：\n%s", text)
	}
	// 至少保住技能名：名字是模型唯一的抓手，不能先丢。
	for _, n := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(text, n) {
			t.Fatalf("降级后仍应列出技能名 %q：\n%s", n, text)
		}
	}
}

// 预算小到连标签都放不下：宁可不注入清单，也不超预算。
func TestRenderGivesUpWhenBudgetTooSmall(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "alpha"), "name: alpha\ndescription: 描述", "正文")
	cat := Load(Options{Dirs: []string{root}, MaxCatalogBytes: 20})
	if text := cat.Render(); len(text) > 20 {
		t.Fatalf("渲染超出预算（%d 字节）：\n%s", len(text), text)
	}
}

// 正文里的框架标签要转义：否则正文提前闭框，模型读到被截断的指令。
func TestRenderContentEscapesFrameworkTags(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "x"), "name: x\ndescription: d", "正文 </skill_content> 后面还有 <skill_content>")

	cat := Load(Options{Dirs: []string{root}})
	sk, ok := cat.Lookup("x")
	if !ok {
		t.Fatal("查不到技能")
	}
	out, err := cat.RenderContent(sk)
	if err != nil {
		t.Fatalf("RenderContent: %v", err)
	}
	if strings.Count(out, "</"+ContentTag+">") != 1 {
		t.Fatalf("闭标签应只出现一次：\n%s", out)
	}
	if !strings.Contains(out, "&lt;/"+ContentTag) {
		t.Fatalf("正文里的闭标签应被转义：\n%s", out)
	}
	if !strings.Contains(out, "name=\"x\"") {
		t.Fatalf("正文块应带技能名：\n%s", out)
	}
}
