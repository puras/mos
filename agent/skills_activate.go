package agent

import (
	"hegq.net/mos/agent/session"
)

// 激活态持久化：本轮已激活的 Skill 列表 + 工具子集并集改存在 *session.Session
// 上（替代 ctx 方案）。原因：tool.Registry.Call 仅返回 (any, error)，ctx 内
// 累积状态对调用方不可见——必须放到 Session 这种可被 caller 持有的对象上。
// driver 在每轮首尾负责清零（sess.TurnSkills = nil; sess.TurnToolSubset = nil）。

// SkillsFromSession 返回 session 当前轮的已激活 Skill 列表（最小切片 session.ActivatedSkill）。
// 无 / nil 时返回 nil。
func SkillsFromSession(sess *session.Session) []session.ActivatedSkill {
	if sess == nil {
		return nil
	}
	return sess.TurnSkills
}

// ToolSubsetFromSession 返回 session 当前轮的工具白名单并集；nil = 未限制。
func ToolSubsetFromSession(sess *session.Session) []string {
	if sess == nil {
		return nil
	}
	return sess.TurnToolSubset
}
