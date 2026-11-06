package skill

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 求交是纯函数：声明了且可见 → 无缺失；不可见 → Missing 按字典序。
func TestCheckAllowedTools(t *testing.T) {
	visible := []string{"read", "shell", "skill"}
	probe := func() []string { return visible }

	res := checkAllowedTools([]string{"read", "shell"}, probe)
	if len(res.Missing) != 0 || res.Notice != "" {
		t.Fatalf("都可见不该有缺失：%+v", res)
	}

	res = checkAllowedTools([]string{"shell", "mcp__gh__pr", "read"}, probe)
	if !reflect.DeepEqual(res.Missing, []string{"mcp__gh__pr"}) {
		t.Fatalf("缺失 = %v", res.Missing)
	}
	if !strings.Contains(res.Notice, "mcp__gh__pr") {
		t.Fatalf("文案应带缺失工具名：%s", res.Notice)
	}
	if !strings.Contains(res.Notice, "不收窄") {
		t.Fatalf("文案应说明只诊断不收窄：%s", res.Notice)
	}
}

// 没声明 / 没探针 → 不诊断（零开销降级）。
func TestCheckAllowedToolsDegrades(t *testing.T) {
	if res := checkAllowedTools(nil, func() []string { return nil }); len(res.Missing) != 0 {
		t.Fatalf("没声明不该诊断：%+v", res)
	}
	if res := checkAllowedTools([]string{"x"}, nil); len(res.Missing) != 0 {
		t.Fatalf("没探针不该诊断：%+v", res)
	}
}

// Catalog 级：探针经 SetToolProbe 注入；可见集是**现查**的（更新后跟上）。
func TestCatalogCheckAllowedTools(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署\nallowed-tools:\n  - shell\n  - deploy_tool", "步骤")

	cat := Load(Options{Dirs: []string{root}})
	sk, ok := cat.Lookup("deploy")
	if !ok {
		t.Fatal("查不到 deploy")
	}

	// 没接探针：不诊断。
	if missing, notice := cat.CheckAllowedTools(sk); missing != nil || notice != "" {
		t.Fatalf("没探针不该诊断：%v %q", missing, notice)
	}

	// 接上探针：deploy_tool 不可见 → 缺失。
	current := []string{"read", "shell"}
	cat.SetToolProbe(func() []string { return current })
	missing, notice := cat.CheckAllowedTools(sk)
	if !reflect.DeepEqual(missing, []string{"deploy_tool"}) {
		t.Fatalf("缺失 = %v", missing)
	}
	if !strings.Contains(notice, "deploy_tool") {
		t.Fatalf("文案 = %s", notice)
	}

	// 可见集变了 → 现查自然跟上（不缓存）。
	current = []string{"read", "shell", "deploy_tool"}
	if missing, _ = cat.CheckAllowedTools(sk); len(missing) != 0 {
		t.Fatalf("可见后不该再报缺失：%v", missing)
	}
}

// E2E：激活技能时，缺失工具写进 mismatch 事件。
func TestToolRecordsAllowedToolsMismatch(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署\nallowed-tools:\n  - gone_tool", "步骤")
	cat := Load(Options{Dirs: []string{root}})
	cat.SetToolProbe(func() []string { return []string{"read"} }) // gone_tool 不可见

	rec := &fakeRecorder{}
	ctx := WithRecorder(context.Background(), rec)
	if _, err := NewTool(cat).Execute(ctx, json.RawMessage(`{"name":"deploy"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rec.mismatchName != "deploy" || !reflect.DeepEqual(rec.missing, []string{"gone_tool"}) {
		t.Fatalf("mismatch = %q %v", rec.mismatchName, rec.missing)
	}
}

// 工具都可见 / 没声明：不记 mismatch。
func TestToolNoMismatchWhenAllVisible(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署\nallowed-tools:\n  - read", "步骤")
	cat := Load(Options{Dirs: []string{root}})
	cat.SetToolProbe(func() []string { return []string{"read", "shell"} })

	rec := &fakeRecorder{}
	ctx := WithRecorder(context.Background(), rec)
	if _, err := NewTool(cat).Execute(ctx, json.RawMessage(`{"name":"deploy"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rec.mismatchName != "" {
		t.Fatalf("不该记 mismatch：%q", rec.mismatchName)
	}
}

// 诊断只进事件，不进给模型的返回值。
func TestMismatchNotInModelResult(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署\nallowed-tools:\n  - gone_tool", "步骤")
	cat := Load(Options{Dirs: []string{root}})
	cat.SetToolProbe(func() []string { return []string{"read"} })

	out, err := NewTool(cat).Execute(context.Background(), json.RawMessage(`{"name":"deploy"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for k, v := range out.(map[string]any) {
		if s, ok := v.(string); ok && strings.Contains(s, "gone_tool") {
			t.Fatalf("缺失工具名不该出现在模型可见结果（%s）里：%q", k, s)
		}
	}
}
