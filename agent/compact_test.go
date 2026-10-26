package agent

import (
	"strings"
	"testing"

	"github.com/puras/mos/llm"
	"github.com/puras/mos/session"
)

func msg(role llm.Role, content string) llm.Message {
	return llm.Message{Role: role, Content: content}
}

func stateWith(facts ...string) *session.TaskState {
	st := &session.TaskState{Goal: "目标", Status: session.StatusRunning}
	for _, f := range facts {
		st.Facts = append(st.Facts, session.Fact{Tool: "read_file", Summary: f})
	}
	return st
}

func TestCompactNoOpUnderBudget(t *testing.T) {
	c := NewDefaultCompactor(CompactorConfig{KeepRecent: 12})
	hist := []llm.Message{msg(llm.RoleSystem, "sys"), msg(llm.RoleUser, "hi"), msg(llm.RoleAssistant, "yo")}
	after, ok := c.Compact(hist, stateWith())
	if ok || len(after) != 3 {
		t.Fatalf("预算内不该压缩: ok=%v n=%d", ok, len(after))
	}
}

func TestCompactShadowsOldKeepsSystem(t *testing.T) {
	c := NewDefaultCompactor(CompactorConfig{KeepRecent: 4})
	hist := []llm.Message{msg(llm.RoleSystem, "sys")}
	for i := 0; i < 10; i++ {
		hist = append(hist, msg(llm.RoleUser, strings.Repeat("u", i+1)))
		hist = append(hist, msg(llm.RoleAssistant, strings.Repeat("a", i+1)))
	}
	after, ok := c.Compact(hist, stateWith("事实甲"))
	if !ok {
		t.Fatal("应发生压缩")
	}
	// system + 摘要 + 4 条保留
	if len(after) != 1+1+4 {
		t.Fatalf("压缩后 = %d 条, want 6: %+v", len(after), after)
	}
	if after[0].Role != llm.RoleSystem || after[0].Content != "sys" {
		t.Fatalf("system 应原样保留在首位: %+v", after[0])
	}
	if after[1].Role != llm.RoleUser || !strings.Contains(after[1].Content, "[任务进度摘要]") {
		t.Fatalf("第 2 条应为摘要: %+v", after[1])
	}
	if !strings.Contains(after[1].Content, "任务目标: 目标") || !strings.Contains(after[1].Content, "- 事实甲") {
		t.Fatalf("摘要应是 TaskState 快照:\n%s", after[1].Content)
	}
	// 保留区是历史的尾部（第 9 个 user 起的 4 条）
	if after[2].Content != strings.Repeat("u", 9) {
		t.Fatalf("保留区应从尾部第 4 条起: %+v", after[2])
	}
}

func TestCompactDoesNotSplitToolCallPair(t *testing.T) {
	c := NewDefaultCompactor(CompactorConfig{KeepRecent: 3})
	// u, a(toolcalls), tool, a(toolcalls), tool, a → 尾部保留 3 条会切在 tool 上
	hist := []llm.Message{
		msg(llm.RoleUser, "q1"),
		{Role: llm.RoleAssistant, Content: "r1", ToolCalls: []llm.ToolCall{{ID: "c1"}}},
		{Role: llm.RoleTool, Content: "res1", ToolCallID: "c1"},
		{Role: llm.RoleAssistant, Content: "r2", ToolCalls: []llm.ToolCall{{ID: "c2"}}},
		{Role: llm.RoleTool, Content: "res2", ToolCallID: "c2"},
		msg(llm.RoleUser, "q2"),
	}
	after, ok := c.Compact(hist, stateWith())
	if !ok {
		t.Fatal("应发生压缩")
	}
	kept := keptMessages(t, after)
	// 保留区首条绝不能是 tool 消息（否则它的调用方被切掉了）
	if kept[0].Role == llm.RoleTool {
		t.Fatalf("切割点切断了 ToolCall-Result 对: %+v", after)
	}
	// 成对不变式：每个 tool 消息的前一条必须是带 ToolCalls 的 assistant
	for i, m := range kept {
		if m.Role == llm.RoleTool {
			if i == 0 || kept[i-1].Role != llm.RoleAssistant || len(kept[i-1].ToolCalls) == 0 {
				t.Fatalf("孤儿 tool 消息: %+v", kept)
			}
		}
	}
}

func TestCompactAllPairsNoFakeCompaction(t *testing.T) {
	// 保留区全是成对工具往返、前移后无可丢弃区：放弃压缩，不拿摘要冒充历史
	c := NewDefaultCompactor(CompactorConfig{KeepRecent: 4})
	hist := []llm.Message{
		msg(llm.RoleSystem, "sys"),
		{Role: llm.RoleAssistant, Content: "r1", ToolCalls: []llm.ToolCall{{ID: "c1"}}},
		{Role: llm.RoleTool, Content: "res1", ToolCallID: "c1"},
		{Role: llm.RoleAssistant, Content: "r2", ToolCalls: []llm.ToolCall{{ID: "c2"}}},
		{Role: llm.RoleTool, Content: "res2", ToolCallID: "c2"},
	}
	after, ok := c.Compact(hist, stateWith())
	if ok {
		t.Fatalf("无可丢弃区时应放弃压缩: %+v", after)
	}
}

func TestCompactOnCompactObserved(t *testing.T) {
	var gotSummary string
	var gotShadowed, gotKept int
	c := NewDefaultCompactor(CompactorConfig{
		KeepRecent: 2,
		OnCompact:  func(summary string, shadowed, kept int) { gotSummary, gotShadowed, gotKept = summary, shadowed, kept },
	})
	hist := []llm.Message{
		msg(llm.RoleUser, "old1"),
		msg(llm.RoleAssistant, "old2"),
		msg(llm.RoleUser, "old3"),
		msg(llm.RoleAssistant, "old4"),
		msg(llm.RoleUser, "new1"),
		msg(llm.RoleAssistant, "new2"),
	}
	if _, ok := c.Compact(hist, stateWith("乙")); !ok {
		t.Fatal("应发生压缩")
	}
	if gotShadowed != 4 || gotKept != 2 {
		t.Fatalf("OnCompact = (%d, %d), want (4, 2)", gotShadowed, gotKept)
	}
	if !strings.Contains(gotSummary, "- 乙") {
		t.Fatalf("回调应拿到摘要文本: %q", gotSummary)
	}
}

func TestCompactNilStateHonestSummary(t *testing.T) {
	c := NewDefaultCompactor(CompactorConfig{KeepRecent: 2})
	hist := []llm.Message{msg(llm.RoleUser, "1"), msg(llm.RoleAssistant, "2"), msg(llm.RoleUser, "3"), msg(llm.RoleAssistant, "4"), msg(llm.RoleUser, "5")}
	after, ok := c.Compact(hist, nil)
	if !ok {
		t.Fatal("应发生压缩")
	}
	if !strings.Contains(summaryOf(t, after).Content, "暂无已验证事实") {
		t.Fatalf("nil 状态应诚实说明: %+v", after)
	}
}

// keptMessages 取压缩结果里摘要之后的保留区。
func keptMessages(t *testing.T, after []llm.Message) []llm.Message {
	t.Helper()
	for i, m := range after {
		if strings.Contains(m.Content, "[任务进度摘要]") {
			return after[i+1:]
		}
	}
	t.Fatalf("压缩结果里没有摘要: %+v", after)
	return nil
}

// summaryOf 取压缩注入的摘要消息。
func summaryOf(t *testing.T, after []llm.Message) llm.Message {
	t.Helper()
	for _, m := range after {
		if strings.Contains(m.Content, "[任务进度摘要]") {
			return m
		}
	}
	t.Fatalf("压缩结果里没有摘要: %+v", after)
	return llm.Message{}
}
