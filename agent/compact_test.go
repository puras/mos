package agent

import (
	"strings"
	"testing"

	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
)

// 构造一轮 tool 交互：assistant 调用 + tool 结果
func pair(id, name, content string) []session.InternalMessage {
	return []session.InternalMessage{
		session.AssistantToolMsg("调用"+name, "", []llm.ToolCall{{ID: id, Name: name, Args: `{}`}}),
		session.ToolMsg(id, name, content),
	}
}

func TestCompactWithinThreshold(t *testing.T) {
	c := NewDefaultCompactor(CompactorConfig{KeepRecent: 6})
	hist := []session.InternalMessage{
		session.SystemMsg("你是助手"),
		session.UserMsg("u1"), session.AssistantMsg("a1", ""),
		session.UserMsg("u2"), session.AssistantMsg("a2", ""),
	}
	out, ok := c.Compact(hist, &session.TaskState{Goal: "g"})
	if ok {
		t.Fatalf("未超阈值不应压缩: %+v", out)
	}
}

func TestCompactInjectsSummary(t *testing.T) {
	c := NewDefaultCompactor(CompactorConfig{KeepRecent: 4})
	state := &session.TaskState{Goal: "算加法", Status: session.StatusRunning}
	state.AddFact(session.Fact{Summary: "1+2=3", Tool: "read_file"})

	hist := make([]session.InternalMessage, 0, 10)
	hist = append(hist, session.SystemMsg("你是助手"))
	for i := 0; i < 4; i++ {
		hist = append(hist, session.UserMsg("u"+itoa(i)), session.AssistantMsg("a"+itoa(i), ""))
	}

	out, ok := c.Compact(hist, state)
	if !ok {
		t.Fatalf("超阈值应压缩")
	}
	// system + 摘要 + 保留的最近消息
	if out[0].Role != llm.RoleSystem {
		t.Fatalf("system 应保留: %+v", out[0])
	}
	if !out[1].Summary || out[1].Role != llm.RoleUser {
		t.Fatalf("第二位应为摘要消息: %+v", out[1])
	}
	if !strings.Contains(out[1].Content, "任务目标: 算加法") || !strings.Contains(out[1].Content, "1+2=3") {
		t.Fatalf("摘要应含 session.TaskState 已验证事实: %q", out[1].Content)
	}
	if len(out) != 1+1+4 {
		t.Fatalf("压缩后长度异常: %d", len(out))
	}
}

// 切割点不切断 ToolCall-Result 对：保留区首条为孤儿 tool 结果时前移切割点。
func TestCompactKeepsToolCallResultPair(t *testing.T) {
	c := NewDefaultCompactor(CompactorConfig{KeepRecent: 3})
	hist := []session.InternalMessage{
		session.AssistantToolMsg("c1", "调用1", []llm.ToolCall{{ID: "c1", Name: "grep", Args: `{"p":"x"}`}}),
		session.ToolMsg("c1", "grep", "结果1"),
		session.AssistantToolMsg("c2", "调用2", []llm.ToolCall{{ID: "c2", Name: "grep", Args: `{"p":"y"}`}}),
		session.ToolMsg("c2", "grep", "结果2"),
		session.UserMsg("继续"),
	}
	out, ok := c.Compact(hist, &session.TaskState{Goal: "g"})
	if !ok {
		t.Fatalf("应压缩")
	}
	// kept[0] 不得是孤儿 tool 结果：要么是 assistant 调用，要么是 user
	if out[0].Role == llm.RoleTool {
		t.Fatalf("切割点切断了 ToolCall-Result 对: %+v", out[0])
	}
	// 若以 assistant 开头，其后必须紧跟其 tool 结果
	if out[0].Role == llm.RoleAssistant && len(out[0].ToolCalls) > 0 {
		if len(out) < 2 || out[1].Role != llm.RoleTool || out[1].ToolCallID != out[0].ToolCalls[0].ID {
			t.Fatalf("assistant 调用与其 tool 结果被切断: %+v", out)
		}
	}
}
