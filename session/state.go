package session

import (
	"strings"
)

// TaskStatus 是任务状态机的整体状态。
//
// S6 只派生三档：pending（还没有用户输入）/ running（末轮未收尾，或末轮以
// error / interrupted / terminated 收尾——任务没完成）/ completed（末轮正常
// 收尾）。blocked 留给 M3 的显式阻塞语义（审批挂起、依赖未满足），不由日志
// 猜测——猜出来的状态比没有状态更误导。
type TaskStatus string

const (
	// StatusPending 任务已建会话但尚无用户输入。
	StatusPending TaskStatus = "pending"
	// StatusRunning 任务执行中（或异常中断后未完成——同样不算完成）。
	StatusRunning TaskStatus = "running"
	// StatusBlocked 任务被显式阻塞（M3 预留：审批挂起、依赖未满足；S6 不派生）。
	StatusBlocked TaskStatus = "blocked"
	// StatusCompleted 最近一轮正常收尾。
	StatusCompleted TaskStatus = "completed"
)

// TaskState 是外置可信任务状态：目标 + 已验证事实 + 状态机。
//
// "外置"的含义：它独立于消息历史存在——历史可以被压缩、截断、丢弃，
// 进度事实不随之丢失（压缩摘要正是从它派生的，见 agent.Compactor）。
// 它不存储、可随时重算（见 DeriveTaskState）；Pending / Blocked 两个字段
// 是 M3 多智能体（Manager 角色）的预留位，S6 不派生、默认为空。
type TaskState struct {
	// Goal 是任务目标（首条用户输入）。
	Goal string `json:"goal"`
	// Facts 是已验证事实（按日志顺序，去重）。
	Facts []Fact `json:"facts,omitempty"`
	// Pending 是待办项（M3 预留，S6 恒空）。
	Pending []string `json:"pending,omitempty"`
	// Blocked 是阻塞项（M3 预留，S6 恒空）。
	Blocked []string `json:"blocked,omitempty"`
	// Status 是任务状态机的当前状态。
	Status TaskStatus `json:"status"`
}

// Fact 是一条已验证事实：只来自**白名单工具的环境输出**（审计不变式）。
// 模型在 assistant 消息里的自述永远不进这里——它不是证据，是主张。
type Fact struct {
	// Tool 是产生该事实的工具名。
	Tool string `json:"tool"`
	// Summary 是单行摘要（工具名 + 结果开头），供快照与报告阅读。
	Summary string `json:"summary"`
	// Evidence 是佐证的环境输出原文（截长保留，不是转述）。
	Evidence string `json:"evidence,omitempty"`
}

// TaskState 是外置可信任务状态：目标 + 已验证事实 + 状态机。
//
// 它**不存储**——从事件日志派生（与消息投影、用量汇总同一哲学）：
// 日志是唯一事实来源，TaskState 随时可重算，Restore 后免费恢复，
// Persistence 无需感知它的存在。派生规则：
//
//   - Goal   = 最后一条 user 事件的文本（TaskState 描述的是**进行中的**
//     任务：多主题会话里旧主题早已收尾，不应永久霸占目标位——否则每次
//     压缩注入的摘要都把模型钉死在第一个问题上；单任务会话下两种取法
//     等价）；
//   - Facts  = trusted 工具的无错 tool/result（证据即日志里的回填内容），
//     按 (tool, evidence) 去重、按日志顺序排列；
//   - Status = 见 TaskStatus 注释。
//
// trusted 由宿主经审计门（agent.Auditor）给出：只有"输出本身就是环境回答"
// 的只读工具可信；变更类工具（shell / write_file）的输出是"未核实声明"，
// 要进状态需先用只读工具二次取证。
func DeriveTaskState(events []Event, trusted func(toolName string) bool) *TaskState {
	st := &TaskState{}
	// CallID → 工具名：tool/result 只带 CallID，名字在同 turn 更早的 tool/call 里。
	byCall := make(map[string]string)
	for i := range events {
		ev := &events[i]
		switch ev.Kind {
		case EventToolCall:
			if ev.ToolCall != nil {
				byCall[ev.ToolCall.ID] = ev.ToolCall.Name
			}
		case EventUser:
			// 取最后一条而非首条：快照描述进行中的任务，旧主题收尾后让位
			// （多主题 chat 会话里，"首条 Goal"会把压缩后的模型钉死在
			// 第一个问题上——事故：会话首问"系统提示词是啥"早 completed，
			// 新问题却被摘要带偏到重新取证提示词装配）。
			st.Goal = ev.Text
		case EventToolResult:
			r := ev.ToolResult
			if r == nil || r.IsError {
				continue // 失败结果不是事实
			}
			name := byCall[r.CallID]
			if name == "" || trusted == nil || !trusted(name) {
				continue // 来源不明的回填、未经审计门信任的工具：不进状态
			}
			st.addFact(Fact{
				Tool:     name,
				Summary:  name + " → " + firstLine(r.Content, 120),
				Evidence: truncateRunes(r.Content, 512),
			})
		}
	}
	st.Status = deriveStatus(events)
	return st
}

// deriveStatus 从日志推导整体状态：
//
//   - 只有用户输入、还没发生过任何事 → pending；
//   - 最近一次收尾是 completed → completed；
//   - 其余（末轮开着 / 出错 / 被终止 / 被中断）→ running，都不算完成。
func deriveStatus(events []Event) TaskStatus {
	active := false // 除用户输入外发生过任何事（模型输出、工具、审批、收尾）
	ended, ok := false, false
	lastTurn := 0
	for i := range events {
		ev := &events[i]
		switch ev.Kind {
		case EventTurnStart:
			active = true
			if ev.Turn > lastTurn {
				lastTurn, ended, ok = ev.Turn, false, false
			}
		case EventUser:
			// 新输入 = 开新一轮：之前的收尾不再代表当前状态。
			if ev.Turn > lastTurn {
				lastTurn, ended, ok = ev.Turn, false, false
			}
		case EventAssistant, EventToolCall, EventToolResult, EventApprovalAsked, EventApprovalDecided:
			active = true
		case EventTurnEnd:
			active = true
			if ev.Turn >= lastTurn {
				ended, ok = true, ev.Reason == "completed"
			}
		}
	}
	switch {
	case !active:
		return StatusPending
	case ended && ok:
		return StatusCompleted
	default:
		return StatusRunning
	}
}

// Snapshot 返回任务状态的文本快照：压缩摘要与审计报告共用这一份文本。
// 只陈述已验证的事实，不替模型总结——摘要是状态的投影，不是新的创作。
func (s *TaskState) Snapshot() string {
	var b strings.Builder
	b.WriteString("任务目标: ")
	b.WriteString(s.Goal)
	b.WriteString("\n任务状态: ")
	b.WriteString(string(s.Status))
	if len(s.Pending) > 0 {
		b.WriteString("\n待办: ")
		b.WriteString(strings.Join(s.Pending, "; "))
	}
	if len(s.Blocked) > 0 {
		b.WriteString("\n阻塞: ")
		b.WriteString(strings.Join(s.Blocked, "; "))
	}
	if len(s.Facts) > 0 {
		b.WriteString("\n已验证事实:")
		for _, f := range s.Facts {
			b.WriteString("\n  - ")
			b.WriteString(f.Summary)
		}
	}
	return b.String()
}

// addFact 追加事实，(tool, evidence) 相同的去重——同一工具对同一问题的
// 重复取证只记一次。
func (s *TaskState) addFact(f Fact) {
	for _, exist := range s.Facts {
		if exist.Tool == f.Tool && exist.Evidence == f.Evidence {
			return
		}
	}
	s.Facts = append(s.Facts, f)
}

// firstLine 取第一行并按 rune 截断到 n（摘要单行可读，不夹带换行）。
func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncateRunes(s, n)
}

// truncateRunes 按 rune 截断（多字节字符不切半），超长以 … 收尾。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
