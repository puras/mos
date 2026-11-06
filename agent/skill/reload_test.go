package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 正文**调用时现读**：改了 SKILL.md 不重扫也生效（S8.4-C）。
func TestBodyReadAtCallTime(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "deploy")
	writeSkill(t, dir, "name: deploy\ndescription: 部署流程", "旧步骤")

	cat := Load(Options{Dirs: []string{root}})
	tool := NewTool(cat)

	// 改文件（不 reload）。
	writeSkill(t, dir, "name: deploy\ndescription: 部署流程", "新步骤")

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"deploy"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	m := out.(map[string]any)
	content, _ := m["content"].(string)
	if !strings.Contains(content, "新步骤") {
		t.Fatalf("应读到改后的正文：\n%s", content)
	}
	if strings.Contains(content, "旧步骤") {
		t.Fatalf("不该读到旧正文：\n%s", content)
	}
}

// reload 幂等：目录没变 → 不换快照、不给通知。
func TestReloadIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署流程", "正文")

	cat := Load(Options{Dirs: []string{root}})
	res := cat.Reload()
	if res.Changed {
		t.Fatalf("目录没变不该算变化：%+v", res)
	}
	if res.Notice != "" {
		t.Fatalf("没变化不该给通知：%q", res.Notice)
	}
}

// 正文改动**不**改目录摘要（DSH 同款：正文每次都重读，不需要失效）。
func TestReloadIgnoresBodyChanges(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "deploy")
	writeSkill(t, dir, "name: deploy\ndescription: 部署流程", "正文")

	cat := Load(Options{Dirs: []string{root}})
	writeSkill(t, dir, "name: deploy\ndescription: 部署流程", "改了正文")
	if res := cat.Reload(); res.Changed {
		t.Fatalf("只改正文不该算目录变化：%+v", res)
	}
}

// 目录变化 → 换快照 + 给通知（新增 / 移除 / 更新）。
func TestReloadDetectsChanges(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署流程", "正文")
	writeSkill(t, filepath.Join(root, "old"), "name: old\ndescription: 旧的", "正文")

	cat := Load(Options{Dirs: []string{root}})

	// 新增一个、删掉一个、改一个描述。
	writeSkill(t, filepath.Join(root, "fresh"), "name: fresh\ndescription: 新技能", "正文")
	if err := os.RemoveAll(filepath.Join(root, "old")); err != nil {
		t.Fatalf("删目录: %v", err)
	}
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 新描述", "正文")

	res := cat.Reload()
	if !res.Changed {
		t.Fatalf("目录变了应报变化：%+v", res)
	}
	if len(res.Added) != 1 || res.Added[0] != "fresh" {
		t.Fatalf("新增 = %v", res.Added)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "old" {
		t.Fatalf("移除 = %v", res.Removed)
	}
	if len(res.Updated) != 1 || res.Updated[0] != "deploy" {
		t.Fatalf("更新 = %v", res.Updated)
	}
	// 通知是**替换目录**语义：带当前完整清单。
	if !strings.Contains(res.Notice, "fresh") || !strings.Contains(res.Notice, "deploy") {
		t.Fatalf("通知应带当前清单：\n%s", res.Notice)
	}
	if strings.Contains(res.Notice, "- old") {
		t.Fatalf("通知不该再列已移除的技能：\n%s", res.Notice)
	}
	// 新快照生效。
	if _, ok := cat.Lookup("fresh"); !ok {
		t.Fatal("新增技能应生效")
	}
	if _, ok := cat.Lookup("old"); ok {
		t.Fatal("移除的技能不该还在")
	}
}

// 技能被删后，模型调用得到可自纠的错误（不是「不存在」这种死说法）。
func TestRemovedSkillGivesActionableError(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署流程", "正文")

	cat := Load(Options{Dirs: []string{root}})
	tool := NewTool(cat)
	if err := os.RemoveAll(filepath.Join(root, "deploy")); err != nil {
		t.Fatalf("删目录: %v", err)
	}
	cat.Reload()

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"deploy"}`))
	if err == nil {
		t.Fatal("已删除的技能不该加载成功")
	}
	if !strings.Contains(err.Error(), "目录可能已更新") {
		t.Fatalf("错误应提示目录已更新：%v", err)
	}
}

// 快照替换是原子的：重扫期间正在跑的调用仍看到完整的一份。
func TestReloadReplacesSnapshotAtomically(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "a"), "name: a\ndescription: 甲", "正文")
	cat := Load(Options{Dirs: []string{root}})
	before := cat.All()
	cat.Reload()
	after := cat.All()
	if len(before) != len(after) {
		t.Fatalf("重扫前后技能数应一致：%d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Name != after[i].Name {
			t.Fatalf("重扫前后顺序应稳定：%v → %v", before, after)
		}
	}
}

// 坏技能在重扫时降级（跳过 + 诊断），不影响其余技能。
//
// 新增一个坏技能：**有效目录没变**（它压根不在清单里）→ 不通知模型，
// 但诊断要刷新（快照换掉）。
func TestReloadKeepsGoodSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "good"), "name: good\ndescription: 好的", "正文")
	cat := Load(Options{Dirs: []string{root}})

	writeSkill(t, filepath.Join(root, "bad"), "name: Bad_Name\ndescription: 坏的", "正文")
	res := cat.Reload()
	if res.Changed {
		t.Fatalf("坏技能不入清单，不该算目录变化：%+v", res)
	}
	if _, ok := cat.Lookup("good"); !ok {
		t.Fatal("好技能不该受影响")
	}
	if !strings.Contains(strings.Join(cat.Warnings(), "\n"), "kebab-case") {
		t.Fatalf("坏技能应出诊断：%v", cat.Warnings())
	}
}
