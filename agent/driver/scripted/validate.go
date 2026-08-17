package scripted

import (
	"errors"
	"fmt"

	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// validateScript 校验脚本（确定性安全闸，fail-closed）：
// 任一校验不过 → 整份脚本拒绝执行。检查：步数上限 / ID 唯一非空 /
// 工具存在 / Args 参数名合法 / DependsOn 已知且无自依赖 / 无环 / Output 命名唯一。
func validateScript(plan *planner.ScriptedPlan, reg *tool.Registry, maxSteps int) error {
	if plan == nil {
		return errors.New("script: nil plan")
	}
	if len(plan.Steps) == 0 {
		return errors.New("script: no steps")
	}
	if maxSteps > 0 && len(plan.Steps) > maxSteps {
		return fmt.Errorf("script: %d steps exceeds max %d", len(plan.Steps), maxSteps)
	}

	// 工具 → schema（参数名校验用）：Registry.List 返回的 llm.Tool.Parameters
	// 是注册时生成的 JSON Schema（map[string]any）。
	schemas := make(map[string]map[string]any, len(plan.Steps))
	for _, t := range reg.List() {
		if m, ok := t.Parameters.(map[string]any); ok {
			schemas[t.Name] = m
		}
	}

	ids := make(map[string]bool, len(plan.Steps))
	outputs := make(map[string]bool)
	for i := range plan.Steps {
		st := &plan.Steps[i]
		if st.ID == "" {
			return fmt.Errorf("script: step %d: empty id", i)
		}
		if ids[st.ID] {
			return fmt.Errorf("script: duplicate step id %q", st.ID)
		}
		ids[st.ID] = true
		if _, ok := reg.Get(st.Tool); !ok {
			return fmt.Errorf("script: step %q: unknown tool %q", st.ID, st.Tool)
		}
		if st.Output != "" {
			if outputs[st.Output] {
				return fmt.Errorf("script: duplicate output name %q", st.Output)
			}
			outputs[st.Output] = true
		}
		if err := validateArgNames(st, schemas[st.Tool]); err != nil {
			return err
		}
	}

	// 第二遍：DependsOn 已知 + 无自依赖
	for i := range plan.Steps {
		st := &plan.Steps[i]
		seen := make(map[string]bool)
		for _, dep := range st.DependsOn {
			if dep == st.ID {
				return fmt.Errorf("script: step %q self-dependency", st.ID)
			}
			if !ids[dep] {
				return fmt.Errorf("script: step %q depends on unknown step %q", st.ID, dep)
			}
			if seen[dep] {
				return fmt.Errorf("script: step %q duplicate dependency %q", st.ID, dep)
			}
			seen[dep] = true
		}
	}

	// 环检测（Kahn 拓扑排序）
	if hasCycle(plan) {
		return errors.New("script: dependency cycle detected")
	}
	return nil
}

// validateArgNames 校验 Args 键均为工具 schema 声明的属性名。
// schema 无 properties（宽松工具）时跳过；插值引用出现在值中，不在此校验范围。
func validateArgNames(st *planner.ScriptStep, schema map[string]any) error {
	if len(st.Args) == 0 {
		return nil
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil // 宽松 schema，执行期校验
	}
	for k := range st.Args {
		if _, ok := props[k]; !ok {
			return fmt.Errorf("script: step %q: unknown arg %q for tool %q", st.ID, k, st.Tool)
		}
	}
	return nil
}

// hasCycle 用 Kahn 算法检测依赖环（dep 边：step 依赖 dep，即 dep → step）。
func hasCycle(plan *planner.ScriptedPlan) bool {
	indeg := make(map[string]int, len(plan.Steps))
	adj := make(map[string][]string, len(plan.Steps)) // dep → 依赖它的 steps
	for i := range plan.Steps {
		st := &plan.Steps[i]
		indeg[st.ID] = len(st.DependsOn)
		for _, dep := range st.DependsOn {
			adj[dep] = append(adj[dep], st.ID)
		}
	}
	queue := make([]string, 0)
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	count := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		count++
		for _, n := range adj[id] {
			indeg[n]--
			if indeg[n] == 0 {
				queue = append(queue, n)
			}
		}
	}
	return count != len(plan.Steps)
}
