package agent

import (
	"strings"
	"testing"

	"hegq.net/mos/llm"
)

// 翻译边界核心行为：标准消息原样转换、自定义/摘要转 user、exclude 过滤。
func TestConvertToLlm(t *testing.T) {
	msgs := []InternalMessage{
		SystemMsg("你是助手"),
		UserMsg("你好"),
		AssistantMsg("你好！", ""),
	}
	got := convertToLlm(msgs)
	if len(got) != 3 {
		t.Fatalf("标准消息应原样转换，len=%d", len(got))
	}
	if got[0].Role != llm.RoleSystem || got[0].Content != "你是助手" {
		t.Fatalf("system 未原样转换: %+v", got[0])
	}
	if got[1].Role != llm.RoleUser || got[2].Role != llm.RoleAssistant {
		t.Fatalf("user/assistant 角色异常: %+v", got)
	}
}

func TestConvertToLlmExcludeFromContext(t *testing.T) {
	excluded := CustomMsg("internal", "仅供内部参考，不进上下文")
	excluded.ExcludeFromContext = true
	msgs := []InternalMessage{UserMsg("问题"), excluded, AssistantMsg("回答", "")}
	got := convertToLlm(msgs)
	if len(got) != 2 {
		t.Fatalf("exclude 消息应被过滤，len=%d", len(got))
	}
	if strings.Contains(got[0].Content, "仅供内部参考") {
		t.Fatalf("exclude 消息进入上下文: %+v", got)
	}
}

func TestConvertToLlmCustomToUser(t *testing.T) {
	msgs := []InternalMessage{
		UserMsg("问题"),
		CustomMsg("agent-2", "子智能体反馈"),
		CustomMsg("", "无来源标注"),
	}
	got := convertToLlm(msgs)
	if len(got) != 3 {
		t.Fatalf("自定义消息应转 user 保留，len=%d", len(got))
	}
	if got[1].Role != llm.RoleUser || !strings.Contains(got[1].Content, "[agent-2]") {
		t.Fatalf("自定义消息未转 user 且标注来源: %+v", got[1])
	}
	if got[2].Role != llm.RoleUser || !strings.Contains(got[2].Content, "[custom]") {
		t.Fatalf("无来源自定义消息未按 custom 标注: %+v", got[2])
	}
}

func TestConvertToLlmSummaryInjection(t *testing.T) {
	msgs := []InternalMessage{
		SystemMsg("你是助手"),
		UserMsg("长对话…"),
		SummaryMsg("已完成：验证了计算器结果 3；当前进行中：汇总。"),
		AssistantMsg("继续", ""),
	}
	got := convertToLlm(msgs)
	if len(got) != 4 {
		t.Fatalf("摘要应保留并转 user，len=%d", len(got))
	}
	if got[2].Role != llm.RoleUser || !strings.HasPrefix(got[2].Content, "[历史摘要]") {
		t.Fatalf("摘要未转 user 角色: %+v", got[2])
	}
}
func TestConvertToLlmKeepsToolCallFields(t *testing.T) {
	msgs := []InternalMessage{
		AssistantToolMsg("算一下", "先思考", []llm.ToolCall{{ID: "c1", Name: "calculator", Args: `{}`}}),
		ToolMsg("c1", "calculator", "3"),
	}
	got := convertToLlm(msgs)
	if got[0].Role != llm.RoleAssistant || got[0].ReasoningContent != "先思考" {
		t.Fatalf("工具调用消息字段丢失: %+v", got[0])
	}
	if len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != "c1" {
		t.Fatalf("tool_calls 丢失: %+v", got[0])
	}
	if got[1].Role != llm.RoleTool || got[1].ToolCallID != "c1" || got[1].Name != "calculator" {
		t.Fatalf("tool 结果消息字段丢失: %+v", got[1])
	}
}
