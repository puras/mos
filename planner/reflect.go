package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hegq.net/mos/llm"
)

// Review 是反思器对产出的评审结果（Basic Reflection 的 Reflect 阶段）。
type Review struct {
	// Issues 是发现的问题列表；空列表表示评审通过（无需修订）。
	Issues []string `json:"issues"`
	// Score 是可选质量分（0-100）；-1 表示模型未提供评分。
	Score int `json:"score,omitempty"`
}

// Reflector 对产出进行批判性评审（Generate → Reflect → Revise 的 Reflect 阶段）。
// 评审应使用独立上下文（与 Generator 解耦），避免"自我合理化"污染
// （对齐 README M3 Basic Reflection：智能体审智能体）。
type Reflector interface {
	Reflect(ctx context.Context, goal, draft string, history []llm.Message) (Review, error)
}

// NewReflector 创建默认评审器：提示模型输出 JSON 评审结果。
// 解析策略（fail-open，避免循环无法终止）：
//  1. 直接解析 JSON（{"issues":["..."],"score":85}）；
//  2. 剥掉 ```json 围栏后再解析；
//  3. 非 JSON 文本非空时整段作为一条 issue（Basic Reflection 原版语义：有批评即修订）；
//     空文本视为评审通过。
func NewReflector(model llm.ChatModel) Reflector {
	return &reflector{model: model}
}

type reflector struct {
	model llm.ChatModel
}

// Reflect 实现 Reflector。
func (r *reflector) Reflect(ctx context.Context, goal, draft string, history []llm.Message) (Review, error) {
	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: reflectPrompt(goal, draft)})

	resp, err := r.model.Chat(ctx, &llm.Request{Messages: msgs})
	if err != nil {
		return Review{}, fmt.Errorf("planner: reflect: %w", err)
	}
	return parseReview(resp.Message.Content), nil
}

// reflectPrompt 是评审阶段的提示词：只要求输出 JSON 评审结果。
func reflectPrompt(goal, draft string) string {
	return `你是严格的评审员。请评审下面的任务产出，找出需要改进的问题。
要求：
1. 只输出 JSON，不要任何解释或围栏：{"issues":["问题1","问题2"],"score":85}
2. issues 为问题列表，没有问题则输出空数组 []
3. score 为 0-100 的质量评分

任务目标：
` + goal + `

当前产出：
` + draft
}

// parseReview 从模型输出中提取评审结果（fail-open 多级回退，见 NewReflector）。
func parseReview(text string) Review {
	for _, s := range []string{text, stripFence(text)} {
		var r struct {
			Issues []string `json:"issues"`
			Score  *int     `json:"score"`
		}
		if err := json.Unmarshal([]byte(s), &r); err == nil {
			review := Review{Score: -1}
			if r.Score != nil {
				review.Score = *r.Score
			}
			for _, issue := range r.Issues {
				if strings.TrimSpace(issue) != "" {
					review.Issues = append(review.Issues, strings.TrimSpace(issue))
				}
			}
			return review
		}
	}
	// 非 JSON 回退：非空文本整段作为一条 issue（有批评即修订）；空文本视为通过。
	text = strings.TrimSpace(text)
	if text == "" {
		return Review{Score: -1}
	}
	return Review{Issues: []string{text}, Score: -1}
}
