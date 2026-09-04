package agent

import (
	"context"

	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
)

// 激活态持久化：本次 Run 已激活的 Skill 列表 + 工具子集并集改存在 *session.Session
// 上（替代 ctx 方案）。原因：tool.Registry.Call 仅返回 (any, error)，ctx 内
// 累积状态对调用方不可见——必须放到 Session 这种可被 caller 持有的对象上。
//
// 生命周期边界 = driver.Agent.Run / RunStream（所有驱动的统一入口）：起始
// ResetRunSkills、收尾 defer FinalizeRunSkills。放在 Agent 层而非各 Driver 内，
// 因为：
//   - scripted / plan-execute / reflection 经各自 Agent.Run 获得同样语义，其中
//     plan-execute/reflection 的内嵌 ReAct 执行器按步直调裸 ReActDriver、不经过
//     Agent 边界，激活态因此在整个外层 Run 内跨步骤持续（正确语义）；
//   - 若放 react.Run 内层：scripted 会跨 Run 泄漏白名单（安全控制失效）、
//     plan-execute 会退化成 per-step 作用域（轮末清零使 allowed-tools 在模型
//     真正用上技能前即失效）。
//
// 作用域是本次 Run 而非单轮。已知边界：TurnSkills 不进 checkpoint（json:"-"），
// 跨挂起-恢复不保留——resume 重放挂起轮工具批时批内 activate_skill 会重新激活。

// SkillsFromSession 返回 session 本次 Run 已激活的 Skill 列表（最小切片 session.ActivatedSkill）。
// 无 / nil 时返回 nil。
func SkillsFromSession(sess *session.Session) []session.ActivatedSkill {
	if sess == nil {
		return nil
	}
	return sess.TurnSkills
}

// ToolSubsetFromSession 返回 session 本次 Run 的工具白名单并集；nil = 未限制。
func ToolSubsetFromSession(sess *session.Session) []string {
	if sess == nil {
		return nil
	}
	return sess.TurnToolSubset
}

// ResetRunSkills 清零本次 Run 的激活态（Run 起始调用，防止上一个 Run 的
// 白名单跨 Run 泄漏——白名单是安全控制）。
func ResetRunSkills(sess *session.Session) {
	if sess == nil {
		return
	}
	sess.TurnSkills = nil
	sess.TurnToolSubset = nil
}

// FinalizeRunSkills 是 Run 收尾的激活态终结：写 skill/deactivate 事件
// （Names 为本次 Run 激活过的全部 Skill）并清零。无激活 Skill 时为 no-op。
// 由 driver.Agent.Run / RunStream 以 defer 调用（正常完成 / 出错 / 挂起冒泡都会走到），
// 对齐架构 v2 §3.5 Skills 生命周期。
func FinalizeRunSkills(sess *session.Session) {
	if sess == nil || len(sess.TurnSkills) == 0 {
		return
	}
	names := make([]string, len(sess.TurnSkills))
	for i, s := range sess.TurnSkills {
		names[i] = s.Name
	}
	_ = sess.Append(event.SessionEventSkillDeactivate, event.MarshalData(event.SkillDeactivateData{Names: names}))
	sess.TurnSkills = nil
	sess.TurnToolSubset = nil
}

// sessionKey 是 ctx 中 *session.Session 的 key 类型。
// driver 在每次工具执行前调用 WithSession 注入；ActivateSkill 等工具经
// SessionFromCtx 取用（写事件、读本次 Run 的激活态）。nil 也允许注入。
type sessionKey struct{}

// WithSession 把 *session.Session 注入 ctx，供工具执行期间按需取用。
// driver（execpath.ExecuteHook）在 tool/execute 钩子前调用此函数。
func WithSession(ctx context.Context, sess *session.Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, sess)
}

// SessionFromCtx 从 ctx 取 *session.Session；nil 表示未注入（如纯单元测试）。
func SessionFromCtx(ctx context.Context) *session.Session {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(sessionKey{}).(*session.Session)
	return v
}
