package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAgent 在 dir 下写一个 AGENT.md（frontmatter + 正文）。
func writeAgent(t *testing.T, dir, front, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录 %q 失败: %v", dir, err)
	}
	content := "---\n" + front + "\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatalf("写 %q 失败: %v", dir, err)
	}
}

// 目录形态：<subagent-dir>/<name>/AGENT.md。
func TestLoadDiscoversDefinitions(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "reviewer"), "name: reviewer\ndescription: 只读代码审查", "你是审查者。")
	writeAgent(t, filepath.Join(root, "deployer"), "name: deployer\ndescription: 部署流程\nmodel: fast\ntools: [read, grep]", "你是部署者。")

	cat := Load(Options{Dirs: []string{root}})
	if cat.Len() != 2 {
		t.Fatalf("应发现 2 个子代理，实际 %d：%v", cat.Len(), cat.Warnings())
	}
	if len(cat.Warnings()) != 0 {
		t.Fatalf("不该有诊断：%v", cat.Warnings())
	}
	d, ok := cat.Lookup("reviewer")
	if !ok {
		t.Fatal("应能按名查到 reviewer")
	}
	if d.Description != "只读代码审查" {
		t.Fatalf("描述不对：%q", d.Description)
	}
	if !d.HasInstructions {
		t.Fatal("正文非空，应记 HasInstructions")
	}
	if d.InstructionsPath != filepath.Join(root, "reviewer", FileName) {
		t.Fatalf("指令路径不对：%q", d.InstructionsPath)
	}
	dep, _ := cat.Lookup("deployer")
	if dep.Model != "fast" {
		t.Fatalf("model 不对：%q", dep.Model)
	}
	if len(dep.Tools) != 2 || dep.Tools[0] != "read" || dep.Tools[1] != "grep" {
		t.Fatalf("tools 不对：%v", dep.Tools)
	}
	// 名字回退：目录名即子代理名。
	if _, ok := cat.Lookup("reviewer"); !ok {
		t.Fatal("应能查到 reviewer")
	}
}

// name 缺失时回退到目录名（同 Skills 口径）。
func TestLoadFallsBackToDirName(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "reviewer"), "description: 只读代码审查", "正文")

	cat := Load(Options{Dirs: []string{root}})
	d, ok := cat.Lookup("reviewer")
	if !ok {
		t.Fatalf("应回退到目录名：%v", cat.Warnings())
	}
	if d.Name != "reviewer" {
		t.Fatalf("名字应为目录名：%q", d.Name)
	}
}

// 不合法的子代理跳过并出诊断（fail-closed），其余照常装。
func TestLoadSkipsInvalidDefinitions(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "ok"), "name: ok\ndescription: 好的", "正文")
	writeAgent(t, filepath.Join(root, "BadName"), "name: Bad_Name\ndescription: 名字不合法", "正文")
	writeAgent(t, filepath.Join(root, "nodesc"), "name: nodesc", "正文")

	cat := Load(Options{Dirs: []string{root}})
	if cat.Len() != 1 {
		t.Fatalf("只该装 1 个，实际 %d：%v", cat.Len(), cat.Warnings())
	}
	warns := strings.Join(cat.Warnings(), "\n")
	if !strings.Contains(warns, "kebab-case") {
		t.Fatalf("应诊断名字不合法：\n%s", warns)
	}
	if !strings.Contains(warns, "description 缺失") {
		t.Fatalf("应诊断描述缺失：\n%s", warns)
	}
}

// 坏 YAML 与缺 frontmatter 都要跳过并出诊断。
func TestLoadSkipsBrokenFiles(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "bad"), "name: [unclosed\n", "正文")
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, FileName), []byte("没有 frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := Load(Options{Dirs: []string{root}})
	if cat.Len() != 0 {
		t.Fatalf("不该装出子代理：%v", cat.All())
	}
	warns := strings.Join(cat.Warnings(), "\n")
	if !strings.Contains(warns, "frontmatter") {
		t.Fatalf("应诊断 frontmatter 问题：\n%s", warns)
	}
}

// 同名子代理：先到先得，后来的被遮蔽并出诊断。
func TestLoadDuplicateNameFirstWins(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeAgent(t, filepath.Join(first, "reviewer"), "name: reviewer\ndescription: 第一份", "正文")
	writeAgent(t, filepath.Join(second, "reviewer"), "name: reviewer\ndescription: 第二份", "正文")

	cat := Load(Options{Dirs: []string{first, second}})
	if cat.Len() != 1 {
		t.Fatalf("同名只应留一个：%v", cat.All())
	}
	d, _ := cat.Lookup("reviewer")
	if d.Description != "第一份" {
		t.Fatalf("应保留先发现的：%q", d.Description)
	}
	if !strings.Contains(strings.Join(cat.Warnings(), "\n"), "重名") {
		t.Fatalf("应诊断重名：%v", cat.Warnings())
	}
}

// 目录读不动 → 诊断，不拒绝启动。
func TestLoadUnreadableDirWarns(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	cat := Load(Options{Dirs: []string{missing}})
	if cat.Len() != 0 {
		t.Fatalf("不该装出子代理：%v", cat.All())
	}
	if len(cat.Warnings()) == 0 {
		t.Fatal("应诊断目录读不动")
	}
}

// 目录根自己含 AGENT.md → 它就是子代理根（不再下钻）。
func TestLoadRootDirectoryHoldsOneDefinition(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "name: solo\ndescription: 独苗", "正文")

	cat := Load(Options{Dirs: []string{root}})
	if cat.Len() != 1 {
		t.Fatalf("应装 1 个：%v", cat.All())
	}
	if _, ok := cat.Lookup("solo"); !ok {
		t.Fatalf("应查到 solo：%v", cat.All())
	}
}

// 指令正文现读：改了文件立刻生效（不必重扫）。
func TestReadInstructionsIsLive(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "reviewer"), "name: reviewer\ndescription: 审查", "第一版人设")
	cat := Load(Options{Dirs: []string{root}})
	d, _ := cat.Lookup("reviewer")

	got, err := cat.ReadInstructions(d)
	if err != nil {
		t.Fatalf("读指令失败: %v", err)
	}
	if got != "第一版人设" {
		t.Fatalf("指令正文不对：%q", got)
	}

	writeAgent(t, filepath.Join(root, "reviewer"), "name: reviewer\ndescription: 审查", "第二版人设")
	got2, err := cat.ReadInstructions(d)
	if err != nil {
		t.Fatalf("读指令失败: %v", err)
	}
	if got2 != "第二版人设" {
		t.Fatalf("正文应现读：%q", got2)
	}
}

// BuildSpec：读正文 + 带声明的白名单与模型覆盖。
func TestBuildSpec(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "reviewer"), "name: reviewer\ndescription: 审查\nmodel: fast\ntools: [read]", "你是审查者。")
	cat := Load(Options{Dirs: []string{root}})
	d, _ := cat.Lookup("reviewer")

	spec, err := cat.BuildSpec(d, "看看这段代码", "附上的上下文")
	if err != nil {
		t.Fatalf("造 spec 失败: %v", err)
	}
	if spec.Name != "reviewer" || spec.Task != "看看这段代码" || spec.Context != "附上的上下文" {
		t.Fatalf("spec 字段不对：%+v", spec)
	}
	if spec.Instructions != "你是审查者。" {
		t.Fatalf("指令正文不对：%q", spec.Instructions)
	}
	if spec.Model != "fast" || len(spec.Tools) != 1 || spec.Tools[0] != "read" {
		t.Fatalf("声明没带过来：%+v", spec)
	}
}

// 定义文件读不动 → 报错（fail-closed），不返回半截 spec。
func TestBuildSpecFailsWhenDefinitionUnreadable(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "reviewer"), "name: reviewer\ndescription: 审查", "正文")
	cat := Load(Options{Dirs: []string{root}})
	d, _ := cat.Lookup("reviewer")
	if err := os.Remove(d.InstructionsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.BuildSpec(d, "干活", ""); err == nil {
		t.Fatal("定义读不出来应报错")
	}
}

// tools 写成单个字符串 → 当一项（容错，同 Skills 的 asStringList 口径）。
func TestToolsAsSingleString(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "reviewer"), "name: reviewer\ndescription: 审查\ntools: read", "正文")
	cat := Load(Options{Dirs: []string{root}})
	d, _ := cat.Lookup("reviewer")
	if len(d.Tools) != 1 || d.Tools[0] != "read" {
		t.Fatalf("tools 应容错成一项：%v", d.Tools)
	}
}

// 子代理工具名：subagent__<name>。
func TestToolName(t *testing.T) {
	if got := ToolName("reviewer"); got != "subagent__reviewer" {
		t.Fatalf("工具名不对：%q", got)
	}
}
