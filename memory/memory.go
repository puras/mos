// Package memory 提供会话记忆的抽象。
// 工作记忆（对话历史，Memory）与长期记忆（RAG 检索，Retriever）均在本包定义。
package memory

import (
	"context"

	"github.com/puras/mos/llm"
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

// Document 是长期记忆检索（RAG）返回的一个分块，携带来源引用。
type Document struct {
	// ID 是分块唯一标识。
	ID string
	// Source 是来源引用（文件路径 / URL / 会话 ID 等），供模型核对事实出处。
	Source string
	// Content 是分块内容。
	Content string
	// Score 是检索相关度得分（尺度由检索后端定义，越高越相关）。
	Score float64
}

// Retriever 是长期记忆检索器抽象（RAG 扩展点）：
// 按查询返回最相关的知识分块，供智能体注入上下文前做知识守门（源验证 / 冲突裁决）。
// 当前仅保留接口与 agent 注入钩子（每轮按任务目标检索注入），
// 具体实现（如 sqlite/FTS/向量混合检索）由使用方按需提供，未注入时行为不变。
type Retriever interface {
	// Retrieve 按查询检索，返回至多 limit 条相关分块（按相关度降序）。
	Retrieve(ctx context.Context, query string, limit int) ([]Document, error)
}
