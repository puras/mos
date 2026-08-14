package agent

import "hegq.net/mos/llm"

// InternalMessage 是 Agent 内部的两层消息结构中的第一层（M2.4 翻译边界）。
// 相比 llm.Message（LLM 标准消息，第二层），附加运行期元数据：
//   - ExcludeFromContext：不进模型上下文，仅保留在会话历史中（供审计/落盘/内部使用）；
//     翻译边界（convertToLlm）会将其排除。元数据是运行期标记，不随 memory 持久化。
//   - Summary：历史摘要消息（M2.5 压缩产物），注入时统一转 user 角色。
//   - Source：消息来源标注（观测/调试），如 user / assistant / tool / system / summary / custom。
type InternalMessage struct {
	llm.Message
	ExcludeFromContext bool   `json:"exclude_from_context,omitempty"`
	Summary            bool   `json:"summary,omitempty"`
	Source             string `json:"source,omitempty"`
}

// SystemMsg 构造系统引导消息。
func SystemMsg(content string) InternalMessage {
	return InternalMessage{Message: llm.Message{Role: llm.RoleSystem, Content: content}, Source: "system"}
}

// UserMsg 构造用户消息。
func UserMsg(content string) InternalMessage {
	return InternalMessage{Message: llm.Message{Role: llm.RoleUser, Content: content}, Source: "user"}
}

// AssistantMsg 构造助手消息；reasoning 为可选的推理内容（thinking 模式须原样回传）。
func AssistantMsg(content, reasoning string) InternalMessage {
	return InternalMessage{Message: llm.Message{Role: llm.RoleAssistant, Content: content, ReasoningContent: reasoning}, Source: "assistant"}
}

// AssistantToolMsg 构造含工具调用请求的助手消息。
func AssistantToolMsg(content, reasoning string, calls []llm.ToolCall) InternalMessage {
	return InternalMessage{Message: llm.Message{Role: llm.RoleAssistant, Content: content, ReasoningContent: reasoning, ToolCalls: calls}, Source: "assistant"}
}

// ToolMsg 构造工具执行结果消息，关联 assistant 消息中的 ToolCall.ID。
func ToolMsg(callID, name, content string) InternalMessage {
	return InternalMessage{Message: llm.Message{Role: llm.RoleTool, Content: content, ToolCallID: callID, Name: name}, Source: "tool"}
}

// SummaryMsg 构造历史摘要消息（M2.5 压缩产物）。
// 内部角色为 user，翻译边界（convertToLlm）将其作为摘要注入，统一转 user 角色。
func SummaryMsg(content string) InternalMessage {
	return InternalMessage{Message: llm.Message{Role: llm.RoleUser, Content: content}, Summary: true, Source: "summary"}
}

// CustomMsg 构造自定义消息（应用层注入的附加上下文、Agent 间通信等）。
// 翻译边界将其统一转 user 角色，并按 source 标注来源，避免污染标准角色序。
func CustomMsg(source, content string) InternalMessage {
	return InternalMessage{Message: llm.Message{Role: "", Content: content}, Source: source}
}

// convertToLlm 是两层消息的翻译边界（M2.4）：
// 将内部结构化消息翻译为 LLM 标准消息（llm.Message），发送给规划器/模型前统一执行。
// 规则：
//  1. 过滤 ExcludeFromContext 的消息（不进模型上下文）。
//  2. 标准角色（system/user/assistant/tool）原样转换。
//  3. 摘要消息（Summary）与自定义消息统一转 user 角色，并按来源标注格式。
func convertToLlm(msgs []InternalMessage) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		if m.ExcludeFromContext {
			continue
		}
		out = append(out, m.translate())
	}
	return out
}

// translate 将单条内部消息翻译为 LLM 标准消息。
func (m *InternalMessage) translate() llm.Message {
	// 摘要消息优先：无论内部角色，统一作为摘要转 user 角色注入
	if m.Summary {
		return llm.Message{Role: llm.RoleUser, Content: "[历史摘要]\n" + m.Content}
	}
	switch m.Role {
	case llm.RoleSystem, llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
		return m.Message
	}
	source := m.Source
	if source == "" {
		source = "custom"
	}
	return llm.Message{Role: llm.RoleUser, Content: "[" + source + "]\n" + m.Content}
}
