package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hegq.net/mos/llm"
)

// ScriptStep 是脚本中的一步（直接工具调用）。
type ScriptStep struct {
	ID        string         `json:"id"`                   // 步骤唯一标识，供 depends_on / output 引用
	Tool      string         `json:"tool"`                 // 工具名（必须在 Registry 中）
	Args      map[string]any `json:"args,omitempty"`       // 参数，可用 ${id} / ${id.field} 引用上游输出
	DependsOn []string       `json:"depends_on,omitempty"` // 依赖的步骤 ID
	Output    string         `json:"output,omitempty"`     // 命名输出，供下游引用（可选）
}

// ScriptedPlan 是脚本阶段的产出：目标 + 带依赖的步骤图。
type ScriptedPlan struct {
	Goal  string       `json:"goal"`
	Steps []ScriptStep `json:"steps"`
}

// ScriptPlanner 生成执行脚本（Scripted 驱动）。
// tools 为当前注册的工具清单：注入名称与描述，防模型编造工具名。
type ScriptPlanner interface {
	Script(ctx context.Context, goal string, history []llm.Message, tools []llm.Tool) (ScriptedPlan, error)
}

// ScriptState 是脚本执行状态的快照（M2.8 checkpoint 载荷）：
// 脚本 + 已完成步骤的命名输出 + 每步状态。resume 时恢复 outputs/status 续跑，不重跑已完成步骤。
type ScriptState struct {
	Plan    ScriptedPlan      `json:"plan"`
	Outputs map[string]any    `json:"outputs,omitempty"`
	Status  map[string]string `json:"status,omitempty"` // stepID → done/skipped/failed/denied/running
}

// NewScript 创建默认脚本规划器：提示模型输出 JSON 脚本。
func NewScript(model llm.ChatModel) ScriptPlanner {
	return &scriptPlanner{model: model}
}

type scriptPlanner struct {
	model llm.ChatModel
}

// Script 实现 ScriptPlanner。
func (p *scriptPlanner) Script(ctx context.Context, goal string, history []llm.Message, tools []llm.Tool) (ScriptedPlan, error) {
	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: scriptPrompt(goal, tools)})

	resp, err := p.model.Chat(ctx, &llm.Request{Messages: msgs})
	if err != nil {
		return ScriptedPlan{}, fmt.Errorf("planner: script: %w", err)
	}
	plan, err := parseScriptPlan(resp.Message.Content)
	if err != nil {
		return ScriptedPlan{}, err
	}
	if plan.Goal == "" {
		plan.Goal = goal
	}
	return plan, nil
}

// scriptPrompt 是脚本阶段的系统提示词：只要求输出 JSON 脚本，工具名限清单内。
func scriptPrompt(goal string, tools []llm.Tool) string {
	var b strings.Builder
	b.WriteString("你是脚本编排器。请将下面的任务目标编排为一个可执行的工具调用脚本。\n")
	b.WriteString("要求：\n")
	b.WriteString("1. 只输出 JSON，不要任何解释或围栏，格式为 {\"steps\":[{\"id\":\"a\",\"tool\":\"工具名\",\"args\":{...},\"depends_on\":[\"a\"],\"output\":\"名字\"}]}\n")
	b.WriteString("2. id 唯一（字母数字下划线），depends_on 只在有真实依赖时声明，output 命名供下游 ${id} 引用\n")
	b.WriteString("3. tool 必须从下面的工具清单中选择，args 用该工具的参数名\n")
	b.WriteString("4. 控制在 3-12 步，每步单意图\n\n")
	b.WriteString("工具清单：\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s：%s\n", t.Name, t.Description)
	}
	b.WriteString("\n目标：\n" + goal)
	return b.String()
}

// parseScriptPlan 从模型输出中解析脚本（失败即报错，不做猜测性解析）。
// 回退：剥 ```json 围栏后再解析；仍失败返回错误（含原始输出）。
func parseScriptPlan(text string) (ScriptedPlan, error) {
	for _, s := range []string{text, stripFence(text)} {
		var plan ScriptedPlan
		if err := json.Unmarshal([]byte(s), &plan); err == nil {
			return plan, nil
		}
	}
	return ScriptedPlan{}, fmt.Errorf("planner: script: 无法解析模型输出为脚本 JSON（原始输出：%q）", truncate(text, 200))
}

// truncate 截断字符串到上限（错误信息用）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
