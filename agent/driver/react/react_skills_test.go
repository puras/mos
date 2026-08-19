package react

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/hook"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/llm"
)

// TestSkillAllowListGuard_BlocksUnlistedTool 验证 skillAllowListGuard 在 ToolSubset
// 已注入 ctx 时拦截未在白名单的工具调用。
func TestSkillAllowListGuard_BlocksUnlistedTool(t *testing.T) {
	guard := skillAllowListGuard()
	ctx := agent.WithToolSubset(context.Background(), []string{"read_file"})

	ev := &hook.HookEvent{ToolCall: &llm.ToolCall{Name: "shell", Args: `{"cmd":"ls"}`}}
	err := guard(ctx, ev)
	if err == nil {
		t.Fatal("expected denial for shell")
	}
	if !strings.Contains(err.Error(), "skill allow-list blocks") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSkillAllowListGuard_AllowsListedTool 验证白名单内工具不被拦截。
func TestSkillAllowListGuard_AllowsListedTool(t *testing.T) {
	guard := skillAllowListGuard()
	ctx := agent.WithToolSubset(context.Background(), []string{"read_file", "shell"})

	ev := &hook.HookEvent{ToolCall: &llm.ToolCall{Name: "shell", Args: `{"cmd":"ls"}`}}
	if err := guard(ctx, ev); err != nil {
		t.Errorf("shell should be allowed: %v", err)
	}
}

// TestSkillAllowListGuard_NoSubset_Noop 验证未声明 allowed-tools（ctx 无 ToolSubset）
// 时 guard 不拦截——保留旧"不限制"语义。
func TestSkillAllowListGuard_NoSubset_Noop(t *testing.T) {
	guard := skillAllowListGuard()
	ctx := context.Background()

	ev := &hook.HookEvent{ToolCall: &llm.ToolCall{Name: "shell", Args: `{"cmd":"ls"}`}}
	if err := guard(ctx, ev); err != nil {
		t.Errorf("expected no-op without subset, got: %v", err)
	}
}

// TestEmitSkillDeactivate_WritesEvent 验证 ctx 有激活 Skill 时 emitSkillDeactivate
// 写 skill/deactivate 事件到 Session Event Log。
func TestEmitSkillDeactivate_WritesEvent(t *testing.T) {
	sess := session.NewSession("test-deactivate")
	ctx := agent.WithActivatedSkills(context.Background(), []agent.Skill{
		{Name: "git", Description: "git helper"},
		{Name: "deploy", Description: "deploy helper"},
	})

	emitSkillDeactivate(ctx, sess)

	if got := lastSkillDeactivateNames(sess); len(got) != 2 || got[0] != "git" || got[1] != "deploy" {
		t.Errorf("lastSkillDeactivateNames = %v, want [git deploy]", got)
	}
}

// TestEmitSkillDeactivate_NoActivatedSkills 验证 ctx 无激活 Skill 时 no-op。
func TestEmitSkillDeactivate_NoActivatedSkills(t *testing.T) {
	sess := session.NewSession("test-deactivate-noop")

	emitSkillDeactivate(context.Background(), sess)

	if got := lastSkillDeactivateNames(sess); len(got) != 0 {
		t.Errorf("expected no event, got %v", got)
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