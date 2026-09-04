package assistant

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/tool"
)

// twoActivateModel 一次返回两个 activate_skill 调用（同一工具批），
// 用于覆盖同批并发激活路径——ActivateSkill 声明 Sequential 前此路径存在数据竞争。
type twoActivateModel struct{}

func (twoActivateModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (twoActivateModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	if hasToolResult(req.Messages) {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "两个技能都已激活"}}, nil
	}
	return &llm.Response{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: "同时激活两个技能",
		ToolCalls: []llm.ToolCall{
			{ID: "a1", Name: "activate_skill", Args: `{"name":"alpha"}`},
			{ID: "a2", Name: "activate_skill", Args: `{"name":"beta"}`},
		},
	}}, nil
}
func (twoActivateModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// TestE2E_TwoActivateSkillsSameBatch 覆盖模型一次返回多个 activate_skill 的路径：
// ActivateSkill 实现 tool.SequentialTool 使整批回退串行，避免并发改写
// sess.TurnSkills / sess.TurnToolSubset / sess.Events。
//
// 断言事件日志 Seq 连续无重复（并发 append 会破坏 Seq: len(Events)+1 的计算）、
// 两个 skill/activate 都落库、skill/deactivate 的 Names 含两者（Run 收尾并集）。
// 以 -race 运行本测试可直接暴露回归。
func TestE2E_TwoActivateSkillsSameBatch(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"alpha", "beta"} {
		writeSKILL(t, filepath.Join(dir, n, "SKILL.md"),
			"---\nname: "+n+"\ndescription: d\nallowed-tools:\n  - tool_"+n+"\n---\n# "+n+"\n")
	}

	a, err := New(twoActivateModel{}, nil, Config{FileAccess: FileAccessConfig{WorkspaceRoot: t.TempDir()}, SkillsDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess := a.NewSession()
	if _, err := a.Run(context.Background(), sess, "同时激活 alpha 与 beta"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Seq 连续无重复：并发写 Session.Events 会产生重复或跳号
	for i, e := range sess.Events {
		if want := int64(i + 1); e.Seq != want {
			t.Fatalf("Events[%d].Seq = %d, want %d（并发 append 破坏了 Seq）", i, e.Seq, want)
		}
	}

	// 两个 skill/activate 都落库
	activated := map[string]bool{}
	var deactivated []string
	for _, e := range sess.Events {
		switch e.Type {
		case event.SessionEventSkillActivate:
			var d event.SkillActivateData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal skill/activate: %v", err)
			}
			activated[d.Name] = true
		case event.SessionEventSkillDeactivate:
			var d event.SkillDeactivateData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatalf("unmarshal skill/deactivate: %v", err)
			}
			deactivated = d.Names
		}
	}
	if !activated["alpha"] || !activated["beta"] {
		t.Errorf("skill/activate 事件 = %v, want alpha + beta 各一条", activated)
	}

	// Run 收尾 deactivate 的 Names 含本次 Run 全部激活项
	got := map[string]bool{}
	for _, n := range deactivated {
		got[n] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Errorf("skill/deactivate Names = %v, want 含 alpha + beta", deactivated)
	}

	// Run 结束后激活态已清零
	if len(sess.TurnSkills) != 0 || sess.TurnToolSubset != nil {
		t.Errorf("Run 收尾未清零: TurnSkills=%v TurnToolSubset=%v", sess.TurnSkills, sess.TurnToolSubset)
	}
}

// 断言 ActivateSkill 满足 tool.SequentialTool（防止后续重构悄悄摘掉串行标记）。
func TestActivateSkill_IsSequential(t *testing.T) {
	var tl tool.Tool = ActivateSkill{}
	st, ok := tl.(tool.SequentialTool)
	if !ok {
		t.Fatal("ActivateSkill 未实现 tool.SequentialTool——同批并发激活会产生数据竞争")
	}
	if !st.Sequential() {
		t.Error("ActivateSkill.Sequential() = false——同批并发激活会产生数据竞争")
	}
}

