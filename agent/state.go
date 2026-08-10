package agent

import (
	"fmt"
	"strings"
)

// TaskStatus 表示任务状态机的整体状态。
type TaskStatus int

const (
	// StatusPending 任务已创建但尚未开始执行。
	StatusPending TaskStatus = iota
	// StatusRunning 任务执行中。
	StatusRunning
	// StatusBlocked 任务被阻塞（如达到最大迭代次数仍未完成）。
	StatusBlocked
	// StatusCompleted 任务正常完成。
	StatusCompleted
)

// String 返回状态的可读名称。
func (s TaskStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusBlocked:
		return "blocked"
	case StatusCompleted:
		return "completed"
	default:
		return "unknown"
	}
}

// Fact 是一条已验证事实：仅允许被环境独立验证过的工具输出写入任务状态。
type Fact struct {
	// Summary 是事实的简短摘要（工具名 + 关键结果），供审计报告阅读。
	Summary string
	// Evidence 是佐证该事实的环境证据（工具原始输出）。
	Evidence string
	// Tool 是产生该事实的工具名。
	Tool string
}

// TaskState 是外部可信任务状态（single source of truth），独立于消息历史存在。
// 对应长程 Agent 的"审计式状态迁移"：
//   - 只有经 Auditor 独立验证的事实才能写入 Facts，模型自述不算数；
//   - 执行上下文（History）可以丢失、刷新、失败，但任务状态始终保留，进度可恢复。
type TaskState struct {
	// Goal 是任务的原始目标（会话首个输入）。
	Goal string
	// Facts 是已验证事实列表（审计报告 = 跨轮次唯一记忆）。
	Facts []Fact
	// Pending 是尚未解决的待办项（由后续 Manager 角色维护）。
	Pending []string
	// Blocked 是阻塞项（由后续 Manager 角色维护）。
	Blocked []string
	// Status 是任务状态机的当前状态。
	Status TaskStatus
}

// AddFact 追加一条已验证事实；相同工具 + 相同证据的去重。
func (s *TaskState) AddFact(f Fact) {
	for _, exist := range s.Facts {
		if exist.Tool == f.Tool && exist.Evidence == f.Evidence {
			return
		}
	}
	s.Facts = append(s.Facts, f)
}

// Snapshot 返回任务状态的文本摘要，供 fresh-context 压缩与审计报告使用。
func (s *TaskState) Snapshot() string {
	var b strings.Builder
	fmt.Fprintf(&b, "任务目标: %s\n任务状态: %s", s.Goal, s.Status)
	if len(s.Pending) > 0 {
		fmt.Fprintf(&b, "\n待办: %s", strings.Join(s.Pending, "; "))
	}
	if len(s.Blocked) > 0 {
		fmt.Fprintf(&b, "\n阻塞: %s", strings.Join(s.Blocked, "; "))
	}
	if len(s.Facts) > 0 {
		b.WriteString("\n已验证事实:")
		for _, f := range s.Facts {
			fmt.Fprintf(&b, "\n  - %s", f.Summary)
		}
	}
	return b.String()
}
