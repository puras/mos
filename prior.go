package mos

// 审批先验（FR-G3 / S9.1）：「同一轮内不重复问人」的实现。
//
// 恢复所需的事实**已经在会话日志里**（`approval/asked` / `approval/decided`
// 带 `Turn` + `CallID`；`tool/call` 没有配对的 `tool/result` = 悬空调用），
// 这里只补**读（派生）**这一半——不新建 checkpoint 文件、不引入 PI 的
// phase 状态机（ARCHITECTURE §3.17）。
//
// 框架层不依赖 session（治理规则 5：框架不绑宿主形态），这层胶水在装配层。

import (
	"context"

	"github.com/puras/mos/agent/approval"
	"github.com/puras/mos/session"
)

// approvalPrior 返回「先验决定查询」：同一轮、同一次调用上已经有人批过，
// **且那次调用还没跑完**（仍悬空）→ 复用决定，不再问人。
//
// 两个条件缺一不可：
//
//   - **有决定**：会话日志里有 `(Turn, CallID)` 对应的 `approval/decided`；
//   - **仍悬空**：那次调用没有 `tool/result`（进程在执行期间被硬杀，
//     结果永远没落盘）。
//
// 「仍悬空」这条是「批准只对本次调用生效」（DSH 的 allowed-once）的护栏：
// 已经跑完的调用**不**借用决定——模型换个参数重发同一个 callId 时必须重新
// 问人，否则一次批准会被放大成无限次。
//
// 命中时复用走的是新 id（审批服务签发），载荷带 `reusedFrom` 指回被复用的
// 那次决定——审计看得出「这次没重新问人」。
func approvalPrior(sess *session.Session) func(context.Context, *session.Session, approval.Request) (approval.PriorDecision, bool) {
	return func(_ context.Context, s *session.Session, req approval.Request) (approval.PriorDecision, bool) {
		if s == nil || req.CallID == "" {
			return approval.PriorDecision{}, false
		}
		d, ok := s.ApprovalDecision(req.Turn, req.CallID)
		if !ok || d.ToolName != req.ToolName {
			return approval.PriorDecision{}, false
		}
		out, ok := approval.ParseOutcome(d.Outcome)
		if !ok {
			return approval.PriorDecision{}, false
		}
		if !isHangingCall(s, req.Turn, req.CallID) {
			return approval.PriorDecision{}, false
		}
		return approval.PriorDecision{Outcome: out, ReusedFrom: d.ID}, true
	}
}

// isHangingCall 判断那次调用是否仍悬空（声明了但没有结果）。
func isHangingCall(s *session.Session, turn int, callID string) bool {
	for _, c := range s.HangingCalls() {
		if c.Turn == turn && c.CallID == callID {
			return true
		}
	}
	return false
}
