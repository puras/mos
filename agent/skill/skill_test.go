package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill 写一个技能：目录 + SKILL.md（frontmatter + 正文）。
func writeSkill(t *testing.T, dir, front, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录 %q: %v", dir, err)
	}
	content := "---\n" + front + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, SkillFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("写技能 %q: %v", dir, err)
	}
}

// names 取技能名列表（装配顺序）。
func names(c *Catalog) []string {
	out := []string{}
	for _, s := range c.All() {
		out = append(out, s.Name)
	}
	return out
}

// 目录下的每个子目录各是一个技能。
func TestLoadFindsSkillsInSubdirectories(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署流程", "步骤 1")
	writeSkill(t, filepath.Join(root, "review"), "name: review\ndescription: 代码评审", "步骤 2")

	cat := Load(Options{Dirs: []string{root}})
	got := names(cat)
	if len(got) != 2 || got[0] != "deploy" || got[1] != "review" {
		t.Fatalf("技能 = %v", got)
	}
	sk, ok := cat.Lookup("deploy")
	if !ok {
		t.Fatal("查不到 deploy")
	}
	body, err := cat.Body(sk)
	if err != nil {
		t.Fatalf("读正文: %v", err)
	}
	if body != "步骤 1" {
		t.Fatalf("正文 = %q", body)
	}
	if sk.Description != "部署流程" {
		t.Fatalf("描述 = %q", sk.Description)
	}
}

// 目录自己含 SKILL.md 时它就是技能根，不再下钻（PI 同款）。
func TestLoadDirItselfIsSkillRoot(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "name: root-skill\ndescription: 根技能", "根正文")
	writeSkill(t, filepath.Join(root, "nested"), "name: nested\ndescription: 嵌套技能", "嵌套正文")

	cat := Load(Options{Dirs: []string{root}})
	got := names(cat)
	if len(got) != 1 || got[0] != "root-skill" {
		t.Fatalf("目录自身是技能根时不该下钻：%v", got)
	}
}

// name 缺失回退到目录名（PI 同款）。
func TestLoadNameFallsBackToDirName(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "my-skill"), "description: 没有 name", "正文")

	cat := Load(Options{Dirs: []string{root}})
	got := names(cat)
	if len(got) != 1 || got[0] != "my-skill" {
		t.Fatalf("技能 = %v", got)
	}
}

// 坏技能降级：跳过 + 出诊断，不拖垮整个装配。
func TestLoadSkipsInvalidSkillsWithWarnings(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "bad"), "name: Bad_Name\ndescription: 名字不合法", "正文")
	writeSkill(t, filepath.Join(root, "nodesc"), "name: nodesc", "正文")
	// 没有 frontmatter 分隔符的文件。
	if err := os.MkdirAll(filepath.Join(root, "nofm"), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "nofm", SkillFileName), []byte("没有 frontmatter 的正文"), 0o644); err != nil {
		t.Fatalf("写技能: %v", err)
	}

	cat := Load(Options{Dirs: []string{root}})
	if cat.Len() != 0 {
		t.Fatalf("不合法的技能不该装上：%v", names(cat))
	}
	w := strings.Join(cat.Warnings(), "\n")
	if !strings.Contains(w, "kebab-case") {
		t.Fatalf("应诊断名字不合法：\n%s", w)
	}
	if !strings.Contains(w, "description 缺失") {
		t.Fatalf("应诊断描述缺失：\n%s", w)
	}
	if !strings.Contains(w, "缺少 frontmatter") {
		t.Fatalf("应诊断缺 frontmatter：\n%s", w)
	}
}

// 同名技能先到先得（目录顺序即优先级），碰撞出诊断。
func TestLoadDuplicateNameKeepsFirst(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(dirA, "x"), "name: dup\ndescription: 甲", "正文甲")
	writeSkill(t, filepath.Join(dirB, "x"), "name: dup\ndescription: 乙", "正文乙")

	cat := Load(Options{Dirs: []string{dirA, dirB}})
	if cat.Len() != 1 {
		t.Fatalf("重名应只留一个：%v", names(cat))
	}
	sk, _ := cat.Lookup("dup")
	body, err := cat.Body(sk)
	if err != nil {
		t.Fatalf("读正文: %v", err)
	}
	if body != "正文甲" {
		t.Fatalf("应保留先发现的：%q", body)
	}
	if !strings.Contains(strings.Join(cat.Warnings(), "\n"), "重名") {
		t.Fatalf("应诊断重名：\n%v", cat.Warnings())
	}
}

// 目录读不动：出诊断，不失败。
func TestLoadMissingDirWarns(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	cat := Load(Options{Dirs: []string{missing}})
	if cat.Len() != 0 {
		t.Fatalf("没有技能：%v", names(cat))
	}
	if !strings.Contains(strings.Join(cat.Warnings(), "\n"), "读不动") {
		t.Fatalf("应诊断目录读不动：\n%v", cat.Warnings())
	}
}

// 正文超预算：**调用时报错**（S8.4-C）——不判死在装配期。
//
// 装配期判死（跳过技能）会让模型以为技能不存在；调用期报错才是真信息。
func TestBodyOverBudgetFailsAtCallTime(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "big"), "name: big\ndescription: 大技能", strings.Repeat("x", 100))

	cat := Load(Options{Dirs: []string{root}, MaxBodyBytes: 10})
	// 技能仍在清单里（模型看得见）——只是加载会失败。
	if cat.Len() != 1 {
		t.Fatalf("超预算的技能不该从清单里消失：%v", names(cat))
	}
	sk, ok := cat.Lookup("big")
	if !ok {
		t.Fatal("查不到 big")
	}
	if _, err := cat.Body(sk); err == nil {
		t.Fatal("正文超预算应报错")
	} else if !strings.Contains(err.Error(), "超预算") {
		t.Fatalf("err = %v", err)
	}
}

// frontmatter 字段全解析：可选字段照收，allowed-tools 只留痕不生效。
func TestLoadParsesOptionalFields(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "full"),
		"name: full\ndescription: 描述\nwhen-to-use: 需要时\nallowed-tools:\n  - read\n  - write\ndisable-model-invocation: true\nmetadata:\n  author: someone",
		"正文")

	cat := Load(Options{Dirs: []string{root}})
	sk, ok := cat.Lookup("full")
	if !ok {
		t.Fatalf("技能没装上：%v", cat.Warnings())
	}
	if sk.WhenToUse != "需要时" {
		t.Fatalf("when-to-use = %q", sk.WhenToUse)
	}
	if len(sk.AllowedTools) != 2 || sk.AllowedTools[0] != "read" {
		t.Fatalf("allowed-tools = %v", sk.AllowedTools)
	}
	if !sk.DisableModelInvocation {
		t.Fatal("disable-model-invocation 没解析出来")
	}
	if sk.Extra["author"] != "someone" {
		t.Fatalf("metadata = %v", sk.Extra)
	}
	// 仅用户可调用 → 不在模型清单里。
	if len(cat.ModelVisible()) != 0 {
		t.Fatalf("仅用户可调用的技能不该模型可见：%v", names(cat))
	}
	if cat.Len() != 1 {
		t.Fatalf("技能总数应含仅用户可调用的：%d", cat.Len())
	}
}

// allowed-tools 只解析、只留痕：诊断里明说当前不生效。
func TestAllowedToolsWarnsNotEffective(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "pre"), "name: pre\ndescription: 预批准\nallowed-tools:\n  - read", "正文")

	cat := Load(Options{Dirs: []string{root}})
	w := strings.Join(cat.Warnings(), "\n")
	if !strings.Contains(w, "allowed-tools") || !strings.Contains(w, "不生效") {
		t.Fatalf("应诊断 allowed-tools 当前不生效：\n%s", w)
	}
}

// 元数据校验：名字与描述的硬约束。
func TestMetadataValidate(t *testing.T) {
	cases := []struct {
		name string
		m    Metadata
		want string
	}{
		{"缺名字", Metadata{Description: "d"}, "name 缺失"},
		{"名字含大写", Metadata{Name: "Deploy", Description: "d"}, "kebab-case"},
		{"名字含下划线", Metadata{Name: "de_ploy", Description: "d"}, "kebab-case"},
		{"缺描述", Metadata{Name: "deploy"}, "description 缺失"},
		{"名字过长", Metadata{Name: strings.Repeat("a", 65), Description: "d"}, "name 过长"},
		{"描述过长", Metadata{Name: "deploy", Description: strings.Repeat("a", 1025)}, "description 过长"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.m.Validate()
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，want 含 %q", err, c.want)
			}
		})
	}
	// 合法样本不报错。
	if err := (Metadata{Name: "deploy-now", Description: "d"}).Validate(); err != nil {
		t.Fatalf("合法元数据不该报错：%v", err)
	}
}
