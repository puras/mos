package react

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
)

// newTestRunContext 构造只含 Session 字段的 *driver.RunContext，供 skillAllowListGuard
// 单元测试使用（无需完整 Agent 配置）。
func newTestRunContext(sess *session.Session) *driver.RunContext {
	return &driver.RunContext{Session: sess}
}

// TestSkillAllowListGuard_BlocksUnlistedTool 验证 skillAllowListGuard 在 sess
// 的 TurnToolSubset 已写入时拦截未在白名单的工具调用。
func TestSkillAllowListGuard_BlocksUnlistedTool(t *testing.T) {
	sess := session.NewSession("test-guard-block")
	sess.TurnToolSubset = []string{"read_file"}

	guard := skillAllowListGuard(newTestRunContext(sess))

	ev := &hook.HookEvent{ToolCall: &llm.ToolCall{Name: "shell", Args: `{"cmd":"ls"}`}}
	err := guard(context.Background(), ev)
	if err == nil {
		t.Fatal("expected denial for shell")
	}
	if !strings.Contains(err.Error(), "skill allow-list blocks") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSkillAllowListGuard_AllowsListedTool 验证白名单内工具不被拦截。
func TestSkillAllowListGuard_AllowsListedTool(t *testing.T) {
	sess := session.NewSession("test-guard-allow")
	sess.TurnToolSubset = []string{"read_file", "shell"}

	guard := skillAllowListGuard(newTestRunContext(sess))

	ev := &hook.HookEvent{ToolCall: &llm.ToolCall{Name: "shell", Args: `{"cmd":"ls"}`}}
	if err := guard(context.Background(), ev); err != nil {
		t.Errorf("shell should be allowed: %v", err)
	}
}

// TestSkillAllowListGuard_NoSubset_Noop 验证未声明 allowed-tools（sess.TurnToolSubset
// 为 nil）时 guard 不拦截——保留"不限制"语义。
func TestSkillAllowListGuard_NoSubset_Noop(t *testing.T) {
	sess := session.NewSession("test-guard-noop")

	guard := skillAllowListGuard(newTestRunContext(sess))

	ev := &hook.HookEvent{ToolCall: &llm.ToolCall{Name: "shell", Args: `{"cmd":"ls"}`}}
	if err := guard(context.Background(), ev); err != nil {
		t.Errorf("expected no-op without subset, got: %v", err)
	}
}

// TestFinalizeRunSkills_WritesEvent 验证 sess.TurnSkills 非空时 agent.FinalizeRunSkills
// 写 skill/deactivate 事件到 Session Event Log，并在写完后清零 TurnSkills +
// TurnToolSubset（下一个 Run 重新累积）。
func TestFinalizeRunSkills_WritesEvent(t *testing.T) {
	sess := session.NewSession("test-deactivate")
	agent.AddTurnSkill(sess, agent.Skill{Name: "git", Description: "git helper"})
	agent.AddTurnSkill(sess, agent.Skill{Name: "deploy", Description: "deploy helper"})
	sess.TurnToolSubset = []string{"git", "shell"}

	agent.FinalizeRunSkills(sess)

	if got := lastSkillDeactivateNames(sess); len(got) != 2 || got[0] != "git" || got[1] != "deploy" {
		t.Errorf("lastSkillDeactivateNames = %v, want [git deploy]", got)
	}
	if len(sess.TurnSkills) != 0 {
		t.Errorf("TurnSkills not cleared after deactivate: %v", sess.TurnSkills)
	}
	if len(sess.TurnToolSubset) != 0 {
		t.Errorf("TurnToolSubset not cleared after deactivate: %v", sess.TurnToolSubset)
	}
}

// TestFinalizeRunSkills_NoActivatedSkills 验证 sess.TurnSkills 为空时
// agent.FinalizeRunSkills no-op（不写事件，也不动 TurnToolSubset）。
func TestFinalizeRunSkills_NoActivatedSkills(t *testing.T) {
	sess := session.NewSession("test-deactivate-noop")
	sess.TurnToolSubset = []string{"untouched"} // 应保持原状

	agent.FinalizeRunSkills(sess)

	if got := lastSkillDeactivateNames(sess); len(got) != 0 {
		t.Errorf("expected no event, got %v", got)
	}
	if len(sess.TurnToolSubset) != 1 || sess.TurnToolSubset[0] != "untouched" {
		t.Errorf("TurnToolSubset should be untouched, got %v", sess.TurnToolSubset)
	}
}

// lastSkillDeactivateNames 扫 Session.Events 提取最后一条 skill/deactivate 的 Names。
func lastSkillDeactivateNames(sess *session.Session) []string {
	var names []string
	for _, e := range sess.Events {
		if e.Type != event.SessionEventSkillDeactivate {
			continue
		}
		var d event.SkillDeactivateData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			continue
		}
		names = d.Names
	}
	return names
}