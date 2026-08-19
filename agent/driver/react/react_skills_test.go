package react

import (
	"context"
	"strings"
	"testing"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/hook"
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