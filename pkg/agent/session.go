package agent

import "hegq.net/mos/pkg/llm"

// Session 表示一次对话会话，持有会话级的历史消息。
// 历史仅含用户/助手/工具消息，system 引导由 Agent 在每次执行时注入。
type Session struct {
	ID      string
	History []llm.Message
}

// NewSession 创建指定 ID 的空会话。
func NewSession(id string) *Session {
	return &Session{ID: id}
}
