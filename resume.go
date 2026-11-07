package mos

// 续跑（FR-G3 / S9.2）：宿主显式指示后，把被中断的那一轮接着跑完。
//
// **默认不自动续跑**（FR-G3 拍板 ①）：框架不拾起挂起的轮次——宿主要先看清
// 现场（`sess.PendingApprovals()` / `sess.HangingCalls()`），再显式调
// `Resume`。没人指示就没人续（fail-closed 取向）。

import (
	"context"
	"errors"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/session"
)

// ResumeOptions 是「续跑」指示（宿主显式给出）。
//
// Turn = 0 时自动选**最后一个被中断的轮次**（有 turn/start 无 turn/end）。
// Calls 为空时续那一轮的**全部**悬空调用。
type ResumeOptions = driver.ResumeOptions

// Resume 续跑：把被中断那一轮里的悬空调用接着走完（重走审批 → 执行 → 回填
// **真实** tool/result）→ 继续问模型 → 本轮正常收尾。
//
// 沿用**原轮次号**：审批决定的复用键是 `(Turn, CallID)`——同一轮里已批过的
// 决定能被复用（不再问人）；走「整轮重跑」（重新 Run）则轮次号变了，必然
// 重新问人。
//
// 没有可续的东西（没有被打断的轮次 / 那一轮没有悬空调用）→ 返回
// `react.ErrNothingToResume`，那一轮**保持挂起**（不收尾——收尾等于把「还欠
// 一次调用」的事实抹掉）。
//
// 续跑不是特权路径：执行照常走 execpath（守卫裁决 / 审批 / 策略都在），
// 工具的外部副作用**可能重复**（文件类靠观察门 CAS 拒；shell 不受保护）。
func (a *Agent) Resume(ctx context.Context, sess *session.Session, opts ResumeOptions) (string, error) {
	if a == nil || a.driver == nil {
		return "", errors.New("mos: 智能体未装配")
	}
	if sess == nil {
		return "", errors.New("mos: 会话未设置")
	}
	ctx = a.prepareCtx(ctx, sess)
	rt := *a.rt
	rt.Concurrency = int(a.concurrency.Load())
	rt.MaxDecisionRounds = int(a.maxRounds.Load())
	// 审批先验（FR-G3 / S9.1）：同一轮同一次调用上已有人批过、且那次调用
	// 还悬空 → 复用决定，不再问人。
	rt.PriorApproval = approvalPrior(sess)
	rt.Resume = &driver.ResumeOptions{Turn: opts.Turn, Calls: opts.Calls}
	// 续跑模式下 input 不参与（那一轮的用户输入已经在日志里）。
	return a.driver.Run(ctx, &rt, sess, "")
}
