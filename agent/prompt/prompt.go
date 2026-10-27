// Package prompt 提供系统提示词的分节装配：Section / Builder + 中心顺序常量。
//
// 设计站位（两个参照项目的中间态，见 docs/ARCHITECTURE.md）：
//   - 对齐 PI 的紧凑：默认只有一句身份 + 两条固定约定，不预设大段话术；
//   - 对齐 DSH 的顺序中心化：分节的**位置**由本包常量统一分配（稳定序利于
//     prompt cache），**文案**归贡献方——工具的跨调用指引写在工具包
//     （tool.Guideline 接口），部署事实归宿主，本包不做任何业务话术。
//
// 三类贡献方、一个汇合点：
//
//	pb := prompt.Default()                            // 框架默认（身份 + 约定）
//	pb.Set(prompt.Section{Name: "identity", ...})     // 宿主覆写默认节
//	pb.AddToolGuidelines(reg.Guidelines())            // 工具自带指引（按注册序）
//	pb.Add(prompt.Section{Order: prompt.OrderRuntime, // 部署事实（cwd 等，宿主自决）
//		Name: "runtime:workdir", Text: ...})
//	rt.SystemPrompt = pb.Render()                     // 组装一次，静态不变
//
// 多智能体（PRD FR-D3）：子智能体的「提示词正文」走 Complete 整段替换——
// prompt.Default().Clone() 后 Complete(body)，其余分节全部让位；多智能体的
// 提示词隔离靠「一实例一 Builder」，不引入 DSH 式 scope 查表。
package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/puras/mos/tool"
)

// 分节顺序常量（DSH 的 SECTION_ORDERS 精简版）：中心分配、全局唯一语义。
// 同一 Order 内按名字字典序稳定排序（确定性输出，利于 prompt cache）。
const (
	// OrderIdentity 是身份句的位置（最先）。
	OrderIdentity = -1000
	// OrderPersona 是宿主人格的位置：在默认身份之后、一切约定之前。
	OrderPersona = 0
	// OrderPolicy 是行为约定的位置（简洁、路径标注这类跨工具约定）。
	OrderPolicy = 500
	// OrderProjectContext 是项目上下文文件（AGENTS.md / CLAUDE.md 等）的位置：
	// 紧跟行为约定之后、工具指引之前——项目约定是对框架约定的特化补充，
	// 紧接着讲最自然；工具指引是能力说明，排在约定之后。
	OrderProjectContext = 750
	// OrderToolBase 是工具指引的起点：AddToolGuidelines 按注册序依次 +1。
	OrderToolBase = 1000
	// OrderRuntime 是易变部署事实的位置（工作目录、审批模式等，宿主自决是否加入）。
	OrderRuntime = 9000
	// OrderSuffix 是收尾补充的位置（最后）。
	OrderSuffix = 10000
)

// Section 是系统提示词的一个分节。
type Section struct {
	// Name 是唯一名（如 "identity"、"tool:shell"、"runtime:workdir"）。
	// 不参与渲染，只用于去重、覆写与排次。
	Name string
	// Order 决定位置；同序按 Name 字典序。
	Order int
	// Text 是分节正文；空文本不参与渲染（剔除而非留空洞）。
	Text string
}

// SectionProjectContext 是项目上下文分节的名字（宿主用 contextfile 装配正文后注入）。
const SectionProjectContext = "project_context"

// Builder 收集分节并渲染最终字符串。零值可直接用；非并发安全——
// 装配发生在启动期（或每个智能体实例各自装配），渲染结果随后只读。
type Builder struct {
	sections    []Section
	complete    string
	hasComplete bool
}

// New 创建空 Builder。
func New() *Builder { return &Builder{} }

// Default 返回 MoS 默认分节（PI 紧凑模板风格）：一句身份 + 两条固定约定。
// 宿主用 Set 覆写内置节、Add 追加自己的节；不想要默认值时用 New 从零装配。
func Default() *Builder {
	b := New()
	mustAdd(b, Section{
		Name:  "identity",
		Order: OrderIdentity,
		Text:  "你是运行在 MoS 智能体框架里的助手，通过调用工具完成用户交代的任务。",
	})
	mustAdd(b, Section{
		Name:  "rules",
		Order: OrderPolicy,
		Text:  "- 回答简洁直接。\n- 涉及文件时写明路径。",
	})
	return b
}

// Add 严格插入一个分节：重名报错（不静默覆盖，装配错误当场暴露）。
// 已 Complete 整段替换后再 Add 也报错——两种装配方式必须二选一。
func (b *Builder) Add(s Section) error {
	if s.Name == "" {
		return fmt.Errorf("prompt: 分节名不能为空")
	}
	if b.hasComplete {
		return fmt.Errorf("prompt: 已 Complete 整段替换，不能再加分节 %q", s.Name)
	}
	for i := range b.sections {
		if b.sections[i].Name == s.Name {
			return fmt.Errorf("prompt: 分节 %q 重复注册（覆写默认节请用 Set）", s.Name)
		}
	}
	b.sections = append(b.sections, s)
	return nil
}

// Set 覆写或新增：同名替换（用于覆写 Default 的内置节），不同名等同 Add。
func (b *Builder) Set(s Section) error {
	if s.Name == "" {
		return fmt.Errorf("prompt: 分节名不能为空")
	}
	if b.hasComplete {
		return fmt.Errorf("prompt: 已 Complete 整段替换，不能再设置分节 %q", s.Name)
	}
	for i := range b.sections {
		if b.sections[i].Name == s.Name {
			b.sections[i] = s
			return nil
		}
	}
	return b.Add(s)
}

// AddProjectContext 注入项目上下文文件正文（AGENTS.md / CLAUDE.md 等，
// 由 agent/contextfile 发现合并）：位置固定在 OrderProjectContext。
//
// 正文为空 = 没有上下文文件：不加分节（不留空洞）。
// 重名即报错——同一个 Builder 注入两次是装配错误，当场暴露。
//
// **顺序**：先加项目约定，再 Complete。Complete 是整段替换（加了它，其余分节
// 全部让位），之后再加分节一律报错（fail-closed，不静默丢弃）。
func (b *Builder) AddProjectContext(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return b.Add(Section{
		Name:  SectionProjectContext,
		Order: OrderProjectContext,
		Text:  text,
	})
}

// Complete 声明整段替换（PRD FR-D3 子智能体「提示词正文」的落点；
// 对齐 DSH 的 complete 段语义）：Render 只返回该文本，已累积分节全部让位。
// 重复 Complete 报错——两个整段替换是装配矛盾（fail-closed）。
func (b *Builder) Complete(text string) error {
	if b.hasComplete {
		return fmt.Errorf("prompt: Complete 重复声明（整段替换只能有一个）")
	}
	b.complete = text
	b.hasComplete = true
	return nil
}

// Clone 派生一个独立副本（子智能体从基础装配出发增删改：按构造遮蔽，
// 不需要 scope 查表）。后续对各自的 Add/Set/Complete 互不影响。
func (b *Builder) Clone() *Builder {
	cp := &Builder{complete: b.complete, hasComplete: b.hasComplete}
	cp.sections = append([]Section(nil), b.sections...)
	return cp
}

// AddToolGuidelines 追加工具自带指引（tool.Registry.Guidelines 的返回值）：
// 名字取 "tool:<工具名>"，Order = OrderToolBase + i——保持注册序
// （Registry 的注册序已稳定，顺序稳定利于 prompt cache）。
func (b *Builder) AddToolGuidelines(gs []tool.GuidelineText) error {
	for i, g := range gs {
		err := b.Add(Section{Name: "tool:" + g.Tool, Order: OrderToolBase + i, Text: g.Text})
		if err != nil {
			return err
		}
	}
	return nil
}

// Render 输出最终提示词：按 (Order, Name) 确定性排序、剔除空文本、空行连接。
// Complete 生效时只返回整段文本。同一 Builder 的 Render 恒等（可重入）。
func (b *Builder) Render() string {
	if b.hasComplete {
		return b.complete
	}
	ss := append([]Section(nil), b.sections...)
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].Order != ss[j].Order {
			return ss[i].Order < ss[j].Order
		}
		return ss[i].Name < ss[j].Name
	})
	parts := make([]string, 0, len(ss))
	for _, s := range ss {
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		parts = append(parts, s.Text)
	}
	return strings.Join(parts, "\n\n")
}

// mustAdd 供 Default 装配内置节：名字与顺序都是本包常量，重名不可能发生；
// 万一未来手滑，启动即 panic 比静默丢节好（装配错误不留到运行时）。
func mustAdd(b *Builder, s Section) {
	if err := b.Add(s); err != nil {
		panic(fmt.Sprintf("prompt: 内置分节装配失败: %v", err))
	}
}
