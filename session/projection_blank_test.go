package session

import (
	"testing"

	"github.com/puras/mos/llm"
)

// 空回复（只有思维链、无正文无工具调用）不进投影：投给模型的历史里留着它，
// 部分厂商会拒收「既无 content 也无 tool_calls」的 assistant 消息（PI 同款
// 跳过）。日志不动——那是发生过的事实。
func TestBlankAssistantNotProjected(t *testing.T) {
	s := New("t", nil)
	mustAppend(t, s, Event{Kind: EventUser, Text: "问题"})
	mustAppend(t, s, Event{Kind: EventAssistant, Reasoning: "想了一想", Usage: &llm.Usage{TotalTokens: 10}})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "答案"})

	msgs := s.Messages()
	if len(msgs) != 2 {
		t.Fatalf("投影 = %d 条, want 2（空回复不进投影）: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || msgs[1].Role != llm.RoleAssistant || msgs[1].Content != "答案" {
		t.Fatalf("投影 = %+v", msgs)
	}
	// 日志里那条空回复仍在（审计凭据：思维链与用量都在里面）
	if evs := s.Events(); len(evs) != 3 {
		t.Fatalf("日志事件数 = %d, want 3（空回复留痕）", len(evs))
	}
}

// 带工具调用的 assistant（正文空）照旧投影——配对不变式靠它。
func TestAssistantWithToolCallsStillProjected(t *testing.T) {
	s := New("t", nil)
	mustAppend(t, s, Event{Kind: EventAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read"}}})

	msgs := s.Messages()
	// 声明了调用却没有回执 → 投影自愈补一条合成中断回执（配对不变式）
	if len(msgs) != 2 {
		t.Fatalf("投影 = %d 条, want 2（assistant + 合成回执）: %+v", len(msgs), msgs)
	}
	if len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].ID != "c1" {
		t.Fatalf("带调用的 assistant 应投影: %+v", msgs[0])
	}
}
