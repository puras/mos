package skill

import (
	"fmt"
	"strings"
)

const (
	// CatalogTag 是技能清单的标签名（进系统提示词）。
	//
	// 用标签而不是裸文本：模型据此知道这段是「可加载技能的清单」，不是
	// 项目约定的一部分。
	CatalogTag = "available_skills"
	// ContentTag 是技能正文的标签名（进工具结果）。
	ContentTag = "skill_content"
)

// Render 渲染技能清单（进系统提示词）。
//
// 清单是**常驻**内容（每轮都在），所以自带字节预算。超预算时三级降级：
// 全量 → 只留技能名（丢描述）→ 逐条塞到装得下为止；丢掉的条数如实写进
// 末尾提示，不静默丢（模型知道还有技能存在，但清单里没列）。
//
// 渲染是**纯函数**：同一份清单渲染两次逐字节一致（装配期渲染一次即固定，
// prompt cache 靠这个）。
func (c *Catalog) Render() string {
	visible := c.ModelVisible()
	if len(visible) == 0 {
		return ""
	}
	budget := c.maxCatalogBytes
	if budget <= 0 {
		budget = DefaultMaxCatalogBytes
	}
	if text := renderCatalog(visible, true, 0); len(text) <= budget {
		return text
	}
	const descNote = "\n（清单预算受限：此处只列技能名，描述略）"
	if text := renderCatalog(visible, false, 0); len(text)+len(descNote) <= budget {
		return text + descNote
	}
	// 装不下：逐条塞到预算为止（预算含末尾的省略提示）。
	kept := 0
	for kept < len(visible) {
		text := renderCatalog(visible[:kept+1], false, len(visible)-kept-1)
		if len(text) > budget {
			break
		}
		kept++
	}
	if text := renderCatalog(visible[:kept], false, len(visible)-kept); len(text) <= budget {
		return text
	}
	// 连一条都列不下（连省略提示都装不下）：退到最小形态，如实说明「没列」。
	minimal := "<" + CatalogTag + ">\n（技能清单过长，未列出）\n</" + CatalogTag + ">"
	if len(minimal) <= budget {
		return minimal
	}
	// 预算小到连标签都放不下：不注入清单（工具仍在，模型按需调用）。
	return ""
}

// renderCatalog 渲染清单正文。withDesc 为 false 时只列名字；
// omitted > 0 时在末尾如实说明有多少条没列出来。
func renderCatalog(skills []Skill, withDesc bool, omitted int) string {
	var b strings.Builder
	b.WriteString("<" + CatalogTag + ">\n")
	b.WriteString("以下技能可按需加载（用 " + ToolName + " 工具按 name 取正文）：\n")
	for _, s := range skills {
		b.WriteString("- " + s.Name)
		if withDesc {
			if s.Description != "" {
				b.WriteString(": " + s.Description)
			}
			if s.WhenToUse != "" {
				b.WriteString("（适用：" + s.WhenToUse + "）")
			}
		}
		b.WriteString("\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "（另有 %d 个技能因清单预算未列出）\n", omitted)
	}
	b.WriteString("</" + CatalogTag + ">")
	return b.String()
}

// RenderContent 渲染技能正文（进工具结果）。
//
// 正文包在标签里：模型据此知道这段是「某个技能的操作说明」，而不是用户
// 的话或环境输出。
func (s Skill) RenderContent() string {
	return fmt.Sprintf("<%s name=%q>\n%s\n</%s>",
		ContentTag, s.Name, escapeTag(s.Body, ContentTag), ContentTag)
}

// escapeTag 转义正文里出现的框架标签序列。
//
// 技能正文不能自己开框或闭框：正文里若出现 </skill_content>，正文块会被
// 提前结束，模型读到的就是被截断的指令——比不转义更糟。
//
// 只转义框架**自己的**标签名，正文其余内容（代码、HTML、尖括号）原样
// 保留：技能正文常带代码示例，全局转义尖括号会把示例改坏。
func escapeTag(body, tag string) string {
	body = strings.ReplaceAll(body, "<"+tag, "&lt;"+tag)
	body = strings.ReplaceAll(body, "</"+tag, "&lt;/"+tag)
	return body
}
