package subagent

import (
	"fmt"
	"sort"
	"strings"
)

// ModelProbe 探测宿主目录里有没有某个模型（装配层注入）。
//
// 存在性判断归宿主：框架看不到宿主的模型目录（那是宿主的事，治理规则 5）。
type ModelProbe func(model string) bool

// CheckModel 校验模型覆盖：声明了但宿主目录里没有 → 报错。
//
// **声明了却没装 = 假能力**（2026-11-06 拍板）：装配期直接报错，不静默降级
// 到父模型——降级等于让模型以为自己在用 A、实际在用 B。
//
// 没接探针（probe = nil）→ 不校验（宿主没提供模型目录，它自己管）。
func CheckModel(def Definition, probe ModelProbe) error {
	model := strings.TrimSpace(def.Model)
	if model == "" || probe == nil {
		return nil
	}
	if !probe(model) {
		return fmt.Errorf("子代理 %q 声明的模型 %q 不在宿主目录里（声明了却没装 = 假能力，不静默降级到父模型）", def.Name, model)
	}
	return nil
}

// ResolveTools 求子代理的**实际**工具面：声明白名单 ∩ 父可见工具集。
//
// - 声明白名单为空 → **不收窄**（继承父的可见集）；
// - 父可见工具集为空（宿主没给）→ 退化为只按声明白名单（宿主自己兜底）；
// - 结果排序输出（确定性）。
//
// **deny 优先**：父可见工具集已经过了宿主 policy 与可见性层（装配层传的
// 就是「模型当前可见的工具名」），所以被拒的工具**进不了**交集——声明
// 不能放大宿主策略（DSH 同款：toolFilter + 父策略向下委派）。
func ResolveTools(declared, parentVisible []string) []string {
	parent := toSet(parentVisible)
	if len(declared) == 0 {
		out := make([]string, 0, len(parentVisible))
		for _, t := range parentVisible {
			out = append(out, t)
		}
		sort.Strings(out)
		return out
	}
	out := make([]string, 0, len(declared))
	for _, t := range declared {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len(parent) == 0 {
			out = append(out, t)
			continue
		}
		if _, ok := parent[t]; ok {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// ParentProfile 描述**父代理**的形状（装配层提供）。
//
// 拆分判据与工具面求交都要拿父的样子当参照（PRD FR-D3：instructions /
// tools / policy 真正改变才拆）。
type ParentProfile struct {
	// Instructions 是父代理的 instructions 正文（父通常是主 Agent 的系统
	// 提示词；这里只用来判断「子代理有没有独立人设」）。
	Instructions string
	// Tools 是父当前可见的工具名集合（已含宿主 policy 与可见性层的收窄）。
	Tools []string
	// Model 是父模型名。
	Model string
}

// SplitDiagnostic 判定子代理**是否真的与父不同**（PRD FR-D3 的拆分判据，
// 可检实现）。
//
// 判据：**任一不同即可**（instructions 非空 / 声明白名单非空且不等于父可见集
// / 声明了模型覆盖）——三者全都没有 → 这个子代理就是父的复制品，拆它
// 只是多一层开销与一层上下文损耗。
//
// 返回值：空串 = 有实质差异（正常）；非空 = 诊断文案（**装配期给一句**，
// 不拦——判据过严会拦掉合法的「只换 instructions」，所以只提示不拒绝）。
//
// 口径说明（§3.18 ⑤）：「instructions 非空」指**子代理的**指令正文非空
// （父的人设与子代理的人设本来就是两份文档，比文本没有意义）；父的
// instructions 只在将来放开「子代理继承父人设」时才用得上。
func SplitDiagnostic(def Definition, parent ParentProfile) string {
	var diffs []string
	if def.HasInstructions {
		diffs = append(diffs, "instructions 非空")
	}
	if len(def.Tools) > 0 && !sameSet(def.Tools, parent.Tools) {
		diffs = append(diffs, "tools 白名单与父可见集不同")
	}
	if strings.TrimSpace(def.Model) != "" && strings.TrimSpace(def.Model) != strings.TrimSpace(parent.Model) {
		diffs = append(diffs, "声明了模型覆盖")
	}
	if len(diffs) == 0 {
		return fmt.Sprintf(
			"子代理 %q 与父无实质差别（instructions / tools / model 三者都没变）——拆开只是多一层开销与一层上下文损耗（PRD FR-D3 拆分判据）",
			def.Name)
	}
	return ""
}

// toSet 把列表转成集合。
func toSet(list []string) map[string]struct{} {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(list))
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

// sameSet 判断两个列表是否同集（忽略顺序与重复）。
func sameSet(a, b []string) bool {
	sa, sb := toSet(a), toSet(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if _, ok := sb[k]; !ok {
			return false
		}
	}
	return true
}
