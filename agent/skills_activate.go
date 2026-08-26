package agent

import (
	"context"

	"hegq.net/mos/agent/session"
)

// 激活态持久化：本次 Run 已激活的 Skill 列表 + 工具子集并集改存在 *session.Session
// 上（替代 ctx 方案）。原因：tool.Registry.Call 仅返回 (any, error)，ctx 内
// 累积状态对调用方不可见——必须放到 Session 这种可被 caller 持有的对象上。
// driver 在 Run 首尾负责清零（sess.TurnSkills = nil; sess.TurnToolSubset = nil）——
// 作用域是本次 Run 而非单轮，见 agent/driver/react.Run 起始处说明。

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
