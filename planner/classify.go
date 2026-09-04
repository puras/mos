package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/puras/mos/llm"
)

// DriverName 是任务路由可选的驱动名（架构 v2 §3.6）。
const (
	DriverReact      = "react"
	DriverPlanExec   = "plan-execute"
	DriverReflection = "reflection"
	// DriverScripted 是脚本化编排驱动（PTC 模式）。
	// opt-in：仅在显式路由规则或 config script_planner 注入时启用，分类器不放行。
	DriverScripted = "scripted"
)

// Classifier 将任务输入分类为合适的驱动（LLM 路由兜底，处理规则未覆盖的模糊输入）。
// 返回 DriverName 之一；无法判断时返回空串（nil 错误），由路由层走 fallback。
type Classifier interface {
	Classify(ctx context.Context, input string, history []llm.Message) (string, error)
}

// NewClassifier 创建默认任务分类器：提示模型输出 JSON 驱动名。
// 解析策略（fail-open，无法分类不阻塞路由）：
//  1. 直接解析 JSON（{"driver":"react"}）；
//  2. 剥掉 ```json 围栏后再解析；
//  3. 无法解析或驱动名未知时返回空串（路由层走 fallback）。
func NewClassifier(model llm.ChatModel) Classifier {
	return &classifier{model: model}
}

type classifier struct {
	model llm.ChatModel
}

// Classify 实现 Classifier。
func (c *classifier) Classify(ctx context.Context, input string, history []llm.Message) (string, error) {
	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: classifyPrompt(input)})

	resp, err := c.model.Chat(ctx, &llm.Request{Messages: msgs})
	if err != nil {
		return "", fmt.Errorf("planner: classify: %w", err)
	}
	return parseClassification(resp.Message.Content), nil
}

// classifyPrompt 是任务分类提示词：只要求输出 JSON 驱动名。
func classifyPrompt(input string) string {
	return `你是任务路由分类器。请判断下面的任务适合哪种智能体循环：
- react：简单直接的任务，一步或几步工具调用即可完成
- plan-execute：多步骤、交付物明确、需要先拆解计划的复杂任务
- reflection：写作、方案、代码等产出型任务，需要自查修订提升质量

只输出 JSON，不要任何解释或围栏：{"driver":"react"}

任务：
` + input
}

// parseClassification 从模型输出中提取驱动名（未知/无法解析返回空串）。
func parseClassification(text string) string {
	for _, s := range []string{text, stripFence(text)} {
		var r struct {
			Driver string `json:"driver"`
		}
		if err := json.Unmarshal([]byte(s), &r); err == nil {
			switch strings.TrimSpace(r.Driver) {
			case DriverReact, DriverPlanExec, DriverReflection:
				return strings.TrimSpace(r.Driver)
			}
		}
	}
	return ""
}
