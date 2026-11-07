package session

// 派生读接口（FR-G3 / S9.1）：「日志即 checkpoint」——恢复所需的事实**已经在
// 日志里**（`approval/asked` / `approval/decided` 带 `Turn` + `CallID`；
// `tool/call` 没有配对的 `tool/result` = 悬空调用），这里只补**读**这一半。
//
// 三条纪律：
//
//   - **纯读**：不写日志、不改状态、不推进序号——派生视图随时可重算；
//   - **不新增事件类型**（治理规则 2 事件预算制）：派生键全部用事件里已有的
//     字段（`Turn` + `CallID` + 审批 id）；
//   - **配对不串**：审批按 **id** 配对（asked 与 decided 同一个 id），悬空调用
//     按 **CallID** 配对（call 与 result 同一个 CallID）——两种配对各走各的，
//     不互相干扰。

// HangingCall 是一次「悬空」的调用：日志里有 `tool/call`，没有配对的
// `tool/result`——进程在执行期间被硬杀（或宿主没写回执）时的残留。
//
// 与投影里的「合成失败回执」是两回事：投影为了保证
// `assistant.ToolCalls` 与 `tool/result` 成对，会给孤儿调用补一条**合成**
// 结果；那是**派生**（给人看的），本接口报告的是**日志事实**（宿主据此决定
// 要不要续跑）。
type HangingCall struct {
	// Turn 是所属轮次（恢复时的匹配键之一）。
	Turn int `json:"turn"`
	// CallID 是调用标识（模型给的；与 tool/result 配对用）。
	CallID string `json:"callId"`
	// ToolName 是工具名。
	ToolName string `json:"toolName"`
	// Args 是调用参数（JSON 原文）。
	Args string `json:"args,omitempty"`
	// Seq 是 tool/call 事件的序号。
	Seq int64 `json:"seq"`
}

// PendingApprovals 返回「问了但还没答」的审批请求（派生：`approval/asked`
// 有、`approval/decided` 无，按 id 配对）。
//
// 顺序按日志顺序（先问的在前）。已决的不在里面——「未决」= 宿主可能还得
// 去问人的那批。
func (s *Session) PendingApprovals() []Approval {
	s.mu.Lock()
	defer s.mu.Unlock()

	asked := make([]Approval, 0, 4)
	decided := make(map[string]bool, 4)
	for i := range s.events {
		ev := &s.events[i]
		if ev.Approval == nil {
			continue
		}
		switch ev.Kind {
		case EventApprovalAsked:
			asked = append(asked, *ev.Approval)
		case EventApprovalDecided:
			decided[ev.Approval.ID] = true
		}
	}
	out := make([]Approval, 0, len(asked))
	for _, a := range asked {
		if !decided[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

// ApprovalOutcome 返回某次审批的结果（`approved` / `rejected` / `cancelled` /
// `unavailable`）。第二个返回值表示「有没有这次决定」。
//
// 按 id 查（不是按轮次）：id 是审批请求的身份（asked 与 decided 共用）。
func (s *Session) ApprovalOutcome(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.events {
		ev := &s.events[i]
		if ev.Kind == EventApprovalDecided && ev.Approval != nil && ev.Approval.ID == id {
			return ev.Approval.Outcome, true
		}
	}
	return "", false
}

// ApprovalDecision 返回「同一轮、同一次调用」上已有的审批决定（先验复用用）。
//
// 匹配键是 `(Turn, CallID)`——**不是 id**：每次审批请求都会签发新 id，
// 拿 id 匹配等于永远匹配不上。同轮同调用上若有多次决定，取**最后一次**
// （最新的一次决定最贴近当下的意图）。
//
// 第二个返回值表示「有没有这次决定」。
func (s *Session) ApprovalDecision(turn int, callID string) (Approval, bool) {
	if callID == "" {
		return Approval{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var out Approval
	found := false
	for i := range s.events {
		ev := &s.events[i]
		if ev.Kind != EventApprovalDecided || ev.Approval == nil {
			continue
		}
		if ev.Turn != turn || ev.Approval.CallID != callID {
			continue
		}
		out = *ev.Approval
		found = true
	}
	return out, found
}

// InterruptedTurn 返回「被中断的那一轮」（有 `turn/start`、且没**正常**收尾的
// 最后一轮）。0 = 没有被中断的轮次。
//
// **中断 = 没结束 或 以 `interrupted` 收尾**：重开会话时 `Restore` 会给孤儿
// turn 合成一条 `turn/end{reason:"interrupted"}`（只补事件、不回写日志）——
// 那一轮**仍然是**被中断的（工具调用悬空），续跑要认它。
//
// 续跑要沿用**同一轮**的轮次号：审批决定的复用键是 `(Turn, CallID)`，轮次号
// 一变就匹配不上（整轮重跑必然重新问人——这是设计如此，不是缺陷）。
func (s *Session) InterruptedTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	started := make(map[int]bool, 4)
	ended := make(map[int]string, 4)
	for i := range s.events {
		ev := &s.events[i]
		switch ev.Kind {
		case EventTurnStart:
			started[ev.Turn] = true
		case EventTurnEnd:
			ended[ev.Turn] = ev.Reason
		}
	}
	best := 0
	for turn := range started {
		if turn <= best {
			continue
		}
		// 正常收尾（completed / error / terminated）的轮次不算被中断。
		if reason, ok := ended[turn]; ok && reason != "interrupted" {
			continue
		}
		best = turn
	}
	return best
}

// HangingCalls 返回「声明了但没有结果」的调用（派生：`tool/call` 有、
// `tool/result` 无，按 CallID 配对）。
//
// 顺序按日志顺序（先声明的在前）。已回填的不在里面。
func (s *Session) HangingCalls() []HangingCall {
	s.mu.Lock()
	defer s.mu.Unlock()

	calls := make([]HangingCall, 0, 4)
	answered := make(map[string]bool, 4)
	for i := range s.events {
		ev := &s.events[i]
		switch ev.Kind {
		case EventToolCall:
			if ev.ToolCall == nil || ev.ToolCall.ID == "" {
				continue
			}
			calls = append(calls, HangingCall{
				Turn:     ev.Turn,
				CallID:   ev.ToolCall.ID,
				ToolName: ev.ToolCall.Name,
				Args:     ev.ToolCall.Args,
				Seq:      ev.Seq,
			})
		case EventToolResult:
			if ev.ToolResult != nil {
				answered[ev.ToolResult.CallID] = true
			}
		}
	}
	out := make([]HangingCall, 0, len(calls))
	for _, c := range calls {
		if !answered[c.CallID] {
			out = append(out, c)
		}
	}
	return out
}
