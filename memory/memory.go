// Package memory 提供会话记忆的抽象。
// 工作记忆（对话历史）在本包定义；长期记忆（向量检索）将在后续里程碑扩展。
package memory

import (
	"context"

	"hegq.net/mos/llm"
)

// Memory 是会话工作记忆的存储抽象。
type Memory interface {
	// Load 加载指定会话的历史消息。
	Load(ctx context.Context, sessionID string) ([]llm.Message, error)
	// Append 向会话追加一条消息。
	Append(ctx context.Context, sessionID string, msg llm.Message) error
	// Clear 清空指定会话的记忆。
	Clear(ctx context.Context, sessionID string) error
}
