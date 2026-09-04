package driver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
)

// fakeScopeDriver 模拟"Run 中途激活技能"的驱动：Run 内激活一个新 Skill，
// 用于验证 Agent.Run 边界的激活态生命周期。
type fakeScopeDriver struct{}

func (fakeScopeDriver) Run(ctx context.Context, rt *RunContext, input string) (string, error) {
	agent.AddTurnSkill(rt.Session, agent.Skill{Name: "inrun"})
	return "done", nil
}

// TestAgent_Run_SkillScopeLifecycle 验证激活态生命周期绑定在 driver.Agent.Run
// 边界（所有驱动的统一入口）：
//   - Run 起始清零上一个 Run 的残留（跨 Run 白名单泄漏——评审 I1）；
//   - Run 收尾 defer 写 skill/deactivate（含本次 Run 新激活项）并清零；
//   - Run 内新激活对守卫可见（跨轮持续——Run 作用域语义）。
//
// 驱动无关：fake 驱动即可复现，scripted / plan-execute 走同一边界。
func TestAgent_Run_SkillScopeLifecycle(t *testing.T) {
	sess := session.NewSession("scope-lifecycle")
	// 模拟上一个 Run 遗留的激活态（如 react 之后接 scripted）
	agent.AddTurnSkill(sess, agent.Skill{Name: "stale"})
	sess.TurnToolSubset = []string{"stale_tool"}

	ag := New(&RunContext{Session: sess}, fakeScopeDriver{})
	if _, err := ag.Run(context.Background(), sess, "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 收尾 deactivate 的 Names = 本次 Run 新激活；上个 Run 的残留已被起始
	// ResetRunSkills 清零（泄漏被拦，残留项由它所属的那个 Run 的边界负责收尾，
	// 不出现在本次 deactivate——否则是幽灵配对）。
	var last []string
	for _, e := range sess.Events {
		if e.Type != event.SessionEventSkillDeactivate {
			continue
		}
		var d event.SkillDeactivateData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		last = d.Names
	}
	if len(last) != 1 || last[0] != "inrun" {
		t.Errorf("skill/deactivate Names = %v, want [inrun]（残留 stale 不跨 Run 配对）", last)
	}

	// 状态已清零
	if len(sess.TurnSkills) != 0 || sess.TurnToolSubset != nil {
		t.Errorf("state not cleared after Run: skills=%v subset=%v", sess.TurnSkills, sess.TurnToolSubset)
	}
}

// TestAgent_RunStream_SkillScopeLifecycle 同上，验证流式路径同语义。
func TestAgent_RunStream_SkillScopeLifecycle(t *testing.T) {
	sess := session.NewSession("scope-lifecycle-stream")
	agent.AddTurnSkill(sess, agent.Skill{Name: "stale"})

	ag := New(&RunContext{Session: sess}, fakeScopeDriver{})
	if _, err := ag.RunStream(context.Background(), sess, "go"); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	if len(sess.TurnSkills) != 0 || sess.TurnToolSubset != nil {
		t.Errorf("state not cleared after RunStream: skills=%v subset=%v", sess.TurnSkills, sess.TurnToolSubset)
	}
	var any bool
	for _, e := range sess.Events {
		if e.Type == event.SessionEventSkillDeactivate {
			any = true
		}
	}
	if !any {
		t.Error("no skill/deactivate event after RunStream")
	}
}
