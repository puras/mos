package agent

import "hegq.net/mos/llm"

// Session 表示一次对话会话，持有会话级的历史消息与可信任务状态。
// 历史仅含用户/助手/工具消息，system 引导由 Agent 在每次执行时注入；
// State 独立于历史存在：历史可压缩/可刷新，已验证进度始终保留。
type Session struct {
	ID      string
	History []llm.Message
	State   TaskState
}

// NewSession 创建指定 ID 的空会话。
func NewSession(id string) *Session {
	return &Session{ID: id}
}
