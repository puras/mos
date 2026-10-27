package session

import (
	"strings"
	"testing"

	"github.com/puras/mos/llm"
)

// compaction 投影：摘要进视图，被遮蔽的消息消失，事件留在日志里。
func TestProjectCompactionShadowing(t *testing.T) {
	// seq: 1=user q1, 2=assistant a1, 3=user q2, 4=assistant a2
	events := []Event{
		{Seq: 1, Kind: EventUser, Text: "q1"},
		{Seq: 2, Kind: EventAssistant, Text: "a1"},
		{Seq: 3, Kind: EventUser, Text: "q2"},
		{Seq: 4, Kind: EventAssistant, Text: "a2"},
		{Seq: 5, Kind: EventCompaction, Compaction: &Compaction{Summary: "摘要甲", FirstKeptSeq: 3}},
		{Seq: 6, Kind: EventUser, Text: "q3"},
	}
	msgs := project(events)
	if len(msgs) != 4 {
		t.Fatalf("投影 = %d 条, want 4（摘要 + q2/a2/q3）: %+v", len(msgs), msgs)
	}
	if !strings.HasPrefix(msgs[0].Content, CompactionSummaryPrefix) || !strings.Contains(msgs[0].Content, "摘要甲") {
		t.Fatalf("首条应为带标记的摘要消息: %+v", msgs[0])
	}
	if msgs[0].Role != llm.RoleUser {
		t.Fatalf("摘要应为 user 角色: %+v", msgs[0])
	}
	if msgs[1].Content != "q2" || msgs[2].Content != "a2" || msgs[3].Content != "q3" {
		t.Fatalf("保留段应是 q2/a2/q3: %+v", msgs)
	}
}

// 迭代压缩：新摘要遮蔽旧摘要（旧摘要已被新摘要折叠，不能重复出现）。
func TestProjectRepeatedCompaction(t *testing.T) {
	events := []Event{
		{Seq: 1, Kind: EventUser, Text: "q1"},
		{Seq: 2, Kind: EventCompaction, Compaction: &Compaction{Summary: "摘要甲", FirstKeptSeq: 2}},
		{Seq: 3, Kind: EventUser, Text: "q2"},
		{Seq: 4, Kind: EventCompaction, Compaction: &Compaction{Summary: "摘要乙", FirstKeptSeq: 4}},
		{Seq: 5, Kind: EventUser, Text: "q3"},
	}
	msgs := project(events)
	if len(msgs) != 2 {
		t.Fatalf("投影 = %d 条, want 2: %+v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0].Content, "摘要乙") || strings.Contains(msgs[0].Content, "摘要甲") {
		t.Fatalf("应只剩新摘要: %+v", msgs[0])
	}
	if msgs[1].Content != "q3" {
		t.Fatalf("保留段: %+v", msgs[1])
	}
}

// View 携带序号：切割下标处的 Seq 就是 FirstKeptSeq 的取值来源。
func TestViewSeqs(t *testing.T) {
	events := []Event{
		{Seq: 7, Kind: EventUser, Text: "q1"},
		{Seq: 8, Kind: EventAssistant, Text: "a1"},
		{Seq: 9, Kind: EventUser, Text: "q2"},
	}
	view := projectSeq(events)
	if len(view) != 3 || view[0].Seq != 7 || view[2].Seq != 9 {
		t.Fatalf("View 序号不对: %+v", view)
	}
}

// 孤儿回执继承声明它的 assistant 事件序号：遮蔽后成对不变式仍成立。
func TestProjectOrphanKeepsPairUnderShadowing(t *testing.T) {
	events := []Event{
		{Seq: 1, Kind: EventUser, Text: "q1"},
		{Seq: 2, Kind: EventAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read"}}},
		// 进程硬杀：result 永远没落盘，直接来了下一轮输入
		{Seq: 3, Kind: EventUser, Text: "q2"},
		{Seq: 4, Kind: EventCompaction, Compaction: &Compaction{Summary: "摘要", FirstKeptSeq: 3}},
	}
	msgs := project(events)
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			t.Fatalf("孤儿回执应随调用方一起被遮蔽: %+v", msgs)
		}
	}
	if len(msgs) != 2 {
		t.Fatalf("投影 = %d 条, want 2（摘要 + q2）: %+v", len(msgs), msgs)
	}
}

// ContextTokens：优先用最后一次真实用量，其后新增事件按 chars/4 估算。
func TestSessionContextTokens(t *testing.T) {
	s := New("t", nil)
	_, _ = s.Append(Event{Kind: EventTurnStart})
	_, _ = s.Append(Event{Kind: EventUser, Text: strings.Repeat("a", 400)})
	_, _ = s.Append(Event{Kind: EventAssistant, Text: "ok", Usage: &llm.Usage{TotalTokens: 500}})
	// 用量之后追加 200 字符的输入 + 一条 tool/call 审计事件（后者不进投影，
	// 不该被计入——工具调用已随 assistant 的 ToolCalls 计过）
	_, _ = s.Append(Event{Kind: EventUser, Text: strings.Repeat("b", 200)})
	_, _ = s.Append(Event{Kind: EventToolCall, ToolCall: &llm.ToolCall{Name: "x", Args: `{"a":"bbbbbbbbbb"}`}})

	if got := s.ContextTokens(); got != 550 {
		t.Fatalf("ContextTokens = %d, want 550（tool/call 审计事件不计）", got)
	}

	// 分项求和兜底（total 缺失时）：fresh + cacheRead + cacheWrite + completion
	s2 := New("t2", nil)
	_, _ = s2.Append(Event{Kind: EventAssistant, Usage: &llm.Usage{
		FreshInputTokens: 100, CompletionTokens: 20,
		Details: llm.UsageDetails{Input: llm.InputTokenDetails{CacheRead: 30, CacheWrite: 50}},
	}})
	if got := s2.ContextTokens(); got != 200 {
		t.Fatalf("分项和 = %d, want 200", got)
	}
}

// compaction 事件的用量计入会话总量（PI：摘要调用的消耗也是会话成本）。
func TestTotalUsageIncludesCompactionUsage(t *testing.T) {
	s := New("t", nil)
	_, _ = s.Append(Event{Kind: EventAssistant, Usage: &llm.Usage{TotalTokens: 100}})
	_, _ = s.Append(Event{Kind: EventCompaction, Compaction: &Compaction{Summary: "s", FirstKeptSeq: 1}, Usage: &llm.Usage{TotalTokens: 42}})
	if got := s.TotalUsage().TotalTokens; got != 142 {
		t.Fatalf("TotalTokens = %d, want 142（含摘要调用）", got)
	}
}
