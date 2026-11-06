package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/puras/mos/tool"
)

// ToolName 是技能工具的名字（模型按名调用）。
const ToolName = "skill"

// Tool 是技能工具：按 name 返回技能正文。
//
// 对齐 DSH 的 tool-skill：参数只有 name，校验模型可调用，返回正文。正文经
// 工具结果进模型上下文 → 进历史；走普通注册表 = 同受守卫、策略、审批与
// tool/call / tool/result 约束（没有旁路）。
type Tool struct {
	catalog *Catalog
}

// NewTool 造技能工具。catalog 为 nil = 没有技能（调用一律失败）。
func NewTool(c *Catalog) *Tool {
	return &Tool{catalog: c}
}

// Name 实现 tool.Tool。
func (t *Tool) Name() string { return ToolName }

// Description 实现 tool.Tool。
//
// 不在这里罗列技能名：清单在系统提示词里（静态），工具描述只说清用法。
// 描述是**每次调用**都会进上下文的东西，重复清单是纯浪费。
func (t *Tool) Description() string {
	return "按 name 加载技能正文（技能清单见系统提示词的 <" + CatalogTag + ">）。" +
		"正文是该技能的操作说明，加载后按里面的步骤执行；用不到的技能别加载——正文占上下文。"
}

// toolParams 是工具参数（只有一个 name，DSH 同款）。
type toolParams struct {
	Name string `json:"name" mos:"desc=技能名（见系统提示词的 available_skills 清单）"`
}

// Parameters 实现 tool.Tool。
func (t *Tool) Parameters() any { return &toolParams{} }

// Execute 实现 tool.Tool：查出技能 → 留痕 → 返回正文。
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[toolParams](args)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return nil, fmt.Errorf("name 不能为空")
	}
	if t == nil || t.catalog == nil {
		return nil, fmt.Errorf("没有可用技能")
	}
	sk, ok := t.catalog.Lookup(name)
	if !ok {
		// 目录可能已经重扫过（技能被删/改名）：提示模型目录已更新，
		// 而不是让它以为技能从来不存在。
		return nil, fmt.Errorf("技能 %q 不在当前技能目录里（目录可能已更新；可用技能见系统提示词的 %s 清单）", name, CatalogTag)
	}
	if sk.DisableModelInvocation {
		// fail-closed：标记为「仅用户显式调用」的技能，模型不许加载。
		return nil, fmt.Errorf("技能 %q 仅限用户显式调用（disable-model-invocation），模型不可加载", name)
	}
	// 正文现读（S8.4-C）：改了 SKILL.md 立刻生效，不重扫、不重启。
	// 读失败 → 不加载（半截指令比没指令更糟）。
	content, err := t.catalog.RenderContent(sk)
	if err != nil {
		return nil, err
	}
	// 留痕：**先事件、后状态**。顺序有讲究——审计写失败就不该留下「已激活」
	// 的痕迹（状态变了、正文没返回 = 一笔糊涂账）。
	// 两处都是可选接线——没接就不记（零开销降级），不报错。
	if rec := RecorderFrom(ctx); rec != nil {
		if err := rec.RecordActivate(sk.Name, sk.Description, sk.Dir); err != nil {
			// 审计写失败 = fail-fast：技能正文会改变模型行为，没有留痕的
			// 加载等于没发生过的加载——宁可不加载。
			return nil, fmt.Errorf("记技能激活事件失败: %w", err)
		}
	}
	if st := StateFrom(ctx); st != nil {
		st.Activate(name)
	}
	return map[string]any{
		"name":    sk.Name,
		"dir":     sk.Dir,
		"content": content,
	}, nil
}
