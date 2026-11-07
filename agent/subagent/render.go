package subagent

import (
	"fmt"
	"strings"
)

const (
	// CatalogTag 是子代理清单的标签名（进系统提示词）。
	//
	// 用标签而不是裸文本：模型据此知道这段是「可派用的子代理清单」，不是
	// 项目约定的一部分（与 Skills 的 available_skills 同一手法，标签名不同：
	// 技能是「知识」，子代理是「角色 + 工具面」）。
	CatalogTag = "available_subagents"
)

// Render 渲染子代理清单（进系统提示词）。
//
// 清单是**常驻**内容（每轮都在），所以自带字节预算。超预算时三级降级：
// 全量 → 只留名字（丢描述）→ 逐条塞到装得下为止；丢掉的条数如实写进末尾
// 提示，不静默丢（模型知道还有子代理存在，只是清单里没列）。
//
// 渲染是**纯函数**：同一份清单渲染两次逐字节一致（装配期渲染一次即固定，
// prompt cache 靠这个）。
func (c *Catalog) Render() string {
	defs := c.All()
	if len(defs) == 0 {
		return ""
	}
	budget := c.maxCatalogBytes
	if budget <= 0 {
		budget = DefaultMaxCatalogBytes
	}
	if text := renderCatalog(defs, true, 0); len(text) <= budget {
		return text
	}
	const descNote = "\n（清单预算受限：此处只列子代理名，描述略）"
	if text := renderCatalog(defs, false, 0); len(text)+len(descNote) <= budget {
		return text + descNote
	}
	// 装不下：逐条塞到预算为止（预算含末尾的省略提示）。
	kept := 0
	for kept < len(defs) {
		text := renderCatalog(defs[:kept+1], false, len(defs)-kept-1)
		if len(text) > budget {
			break
		}
		kept++
	}
	if text := renderCatalog(defs[:kept], false, len(defs)-kept); len(text) <= budget {
		return text
	}
	// 连一条都列不下（连省略提示都装不下）：退到最小形态，如实说明「没列」。
	minimal := "<" + CatalogTag + ">\n（子代理清单过长，未列出）\n</" + CatalogTag + ">"
	if len(minimal) <= budget {
		return minimal
	}
	// 预算小到连标签都放不下：不注入清单（工具仍在，模型按需调用）。
	return ""
}

// renderCatalog 渲染清单正文。withDesc 为 false 时只列名字；omitted > 0 时
// 在末尾如实说明有多少条没列出来。
func renderCatalog(defs []Definition, withDesc bool, omitted int) string {
	var b strings.Builder
	b.WriteString("<" + CatalogTag + ">\n")
	b.WriteString("以下子代理可按需派用（用 " + ToolPrefix + "<name> 工具点名调用）：\n")
	for _, d := range defs {
		b.WriteString("- " + d.Name)
		if withDesc {
			if d.Description != "" {
				b.WriteString(": " + d.Description)
			}
			if len(d.Tools) > 0 {
				b.WriteString("（可用工具：" + strings.Join(d.Tools, "、") + "）")
			}
		}
		b.WriteString("\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "（另有 %d 个子代理因清单预算未列出）\n", omitted)
	}
	b.WriteString("</" + CatalogTag + ">")
	return b.String()
}
