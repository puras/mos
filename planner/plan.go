package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hegq.net/mos/llm"
)

// PlanStep 是计划中的一步（Plan&Execute 执行单元）。
type PlanStep struct {
	Index   int    `json:"index"`
	Summary string `json:"summary"`
}

// Plan 是计划阶段的产出：目标 + 有序步骤列表。
type Plan struct {
	Goal  string     `json:"goal"`
	Steps []PlanStep `json:"steps"`
}

// PlanPlanner 生成任务执行计划（Plan&Execute 驱动的计划阶段，架构 v2 §3.3）。
// history 为当前完整对话（不含 system）；goal 为本次任务目标。
type PlanPlanner interface {
	Plan(ctx context.Context, goal string, history []llm.Message) (Plan, error)
}

// NewPlan 创建默认计划规划器：提示模型输出 JSON 步骤数组。
// 解析策略（fail-open，保证驱动始终可得可执行计划）：
//  1. 直接解析 JSON 数组（[{"summary": "..."}, ...]）；
//  2. 剥掉 ```json 围栏后再解析；
//  3. 按非空行拆分（去 "1." / "- " / "1、" 等序号前缀）；
//  4. 仍无步骤时整体作为单步计划。
func NewPlan(model llm.ChatModel) PlanPlanner {
	return &planPlanner{model: model}
}

type planPlanner struct {
	model llm.ChatModel
}

// Plan 实现 PlanPlanner。
func (p *planPlanner) Plan(ctx context.Context, goal string, history []llm.Message) (Plan, error) {
	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: planPrompt(goal)})

	resp, err := p.model.Chat(ctx, &llm.Request{Messages: msgs})
	if err != nil {
		return Plan{}, fmt.Errorf("planner: plan: %w", err)
	}

	steps := parsePlanSteps(resp.Message.Content)
	if len(steps) == 0 {
		steps = []PlanStep{{Index: 1, Summary: goal}}
	}
	return Plan{Goal: goal, Steps: steps}, nil
}

// planPrompt 是计划阶段的系统提示词：只要求输出 JSON 步骤数组。
func planPrompt(goal string) string {
	return `你是任务规划器。请将下面的任务目标分解为可执行的步骤列表。
要求：
1. 只输出 JSON 数组，不要任何解释或围栏，格式为 [{"summary":"步骤描述"}, ...]
2. 步骤有序依赖，每步可独立交给执行代理完成
3. 控制在 3-8 步，避免过细

目标：
` + goal
}

// parsePlanSteps 从模型输出中提取步骤列表（fail-open 多级回退，见 NewPlan）。
func parsePlanSteps(text string) []PlanStep {
	for _, s := range []string{text, stripFence(text)} {
		var raw []struct {
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(s), &raw); err == nil && len(raw) > 0 {
			steps := make([]PlanStep, 0, len(raw))
			for i, r := range raw {
				if strings.TrimSpace(r.Summary) == "" {
					continue
				}
				steps = append(steps, PlanStep{Index: i + 1, Summary: strings.TrimSpace(r.Summary)})
			}
			if len(steps) > 0 {
				return steps
			}
		}
	}
	// 行拆分回退：每行一步，去掉常见序号前缀。
	var steps []PlanStep
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = stripLinePrefix(line)
		steps = append(steps, PlanStep{Index: len(steps) + 1, Summary: line})
	}
	return steps
}

// stripFence 去掉 ```json ... ``` 或 ``` ... ``` 围栏。
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	for _, open := range []string{"```json", "```"} {
		if strings.HasPrefix(s, open) {
			s = strings.TrimPrefix(s, open)
			break
		}
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

// stripLinePrefix 去掉行首序号/列表前缀（"1."、"1、"、"1)"、"1）"、"- "、"* "）。
func stripLinePrefix(line string) string {
	line = strings.TrimSpace(line)
	runes := []rune(line)
	for _, sep := range []rune{'.', '、', ')', '）'} {
		for i := 1; i <= 3 && i < len(runes); i++ {
			if runes[i] == sep && isDigitRunes(runes[:i]) {
				return strings.TrimSpace(string(runes[i+1:]))
			}
		}
	}
	if len(runes) >= 2 && (runes[0] == '-' || runes[0] == '*') && runes[1] == ' ' {
		return strings.TrimSpace(string(runes[2:]))
	}
	return line
}

func isDigitRunes(s []rune) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
