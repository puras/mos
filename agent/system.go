package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/llm"
)

// SystemSlot 标识系统提示词的一个分段槽位（M2.6 动态组装）。
// 槽位化的意义：固定指令（系统约束）与运行时状态（任务定义 / 当前状态 / 工具签名）
// 分离，后者每轮随 TaskState 进度重新组装，前者保持静态不变以利于 prompt cache。
type SystemSlot int

const (
	// SlotConstraints 系统约束：身份与固定指令。克制主义——保持精简，
	// 固定指令占得越少留给任务上下文越多（参考 Pi 约 1K token 对比 Claude Code 数万）。
	SlotConstraints SystemSlot = iota
	// SlotTask 任务定义：当前目标（sess.State.Goal）。
	SlotTask
	// SlotState 当前状态：TaskState 已验证事实快照（仅环境验证过的事实，模型自述不算数）。
	SlotState
	// SlotTools 工具签名：可用工具清单（name + 一句话描述，紧凑渲染）。
	SlotTools
	// SlotProject 项目指令：CLAUDE.md 向上递归合并（顶层在前、就近覆盖）。
	SlotProject
	// SlotSkills 技能清单：SKILL.md 懒加载目录（frontmatter 的 name + description 触发条件；
	// 正文与 references/ 由模型触发时经 read_file 按需读取）。
	SlotSkills
	// SlotSummary 历史摘要：跨轮压缩摘要。默认不启用——M2.5 压缩摘要已由 Compactor
	// 注入消息历史，系统槽位重复注入会造成冗余；需要时显式启用。
	SlotSummary
)

// defaultEnableSlots 默认启用的槽位：约束 / 任务 / 状态 / 工具签名。
// 项目指令与技能清单依赖目录配置（配置了对应目录才生效），历史摘要默认关闭（见 SlotSummary）。
var defaultEnableSlots = []SystemSlot{SlotConstraints, SlotTask, SlotState, SlotTools}

// SystemSection 是系统提示词的一个分段（标题 + 内容）。
type SystemSection struct {
	Title   string
	Content string
}

// SystemAssembler 按槽位组装系统提示词分段（M2.6 动态组装）。
// 每次模型调用前（每轮）执行，可依据最新的 TaskState 动态产出——
// 当前状态 / 任务定义随进度刷新，固定指令（系统约束）保持不变。
type SystemAssembler interface {
	Assemble(ctx context.Context, sess *session.Session, tools []llm.Tool) ([]SystemSection, error)
}

// SystemAssemblerFunc 是 SystemAssembler 的函数式适配器，
// 便于应用层以闭包形式实现自定义组装器。
type SystemAssemblerFunc func(ctx context.Context, sess *session.Session, tools []llm.Tool) ([]SystemSection, error)

// Assemble 实现 SystemAssembler。
func (f SystemAssemblerFunc) Assemble(ctx context.Context, sess *session.Session, tools []llm.Tool) ([]SystemSection, error) {
	return f(ctx, sess, tools)
}

// SystemAssemblerConfig 是默认组装器的配置。
type SystemAssemblerConfig struct {
	// Constraints 系统约束槽位内容（身份与固定指令）。克制主义：保持精简。
	Constraints string
	// EnableSlots 启用的槽位；为空时使用默认集（约束/任务/状态/工具签名）。
	EnableSlots []SystemSlot
	// ProjectDocDir 项目指令目录：从该目录向上递归查找 CLAUDE.md 合并。
	// 为空时不加载项目指令槽位。
	ProjectDocDir string
	// SkillsDir 技能目录：扫描其中的 SKILL.md（frontmatter 声明 name/description）
	// 生成懒加载清单。为空时不加载技能清单槽位。
	SkillsDir string
}

// NewDefaultSystemAssembler 创建默认的槽位式系统提示词组装器。
// EnableSlots 为空时启用默认集（约束/任务/状态/工具签名），并随配置自动带上
// 项目指令（配置了 ProjectDocDir）与技能清单（配置了 SkillsDir）；显式指定
// EnableSlots 时完全按调用方控制（未列出的槽位不产出，含目录配置的）。
func NewDefaultSystemAssembler(cfg SystemAssemblerConfig) SystemAssembler {
	if len(cfg.EnableSlots) == 0 {
		slots := append([]SystemSlot{}, defaultEnableSlots...)
		if cfg.ProjectDocDir != "" {
			slots = append(slots, SlotProject)
		}
		if cfg.SkillsDir != "" {
			slots = append(slots, SlotSkills)
		}
		cfg.EnableSlots = slots
	}
	return &defaultSystemAssembler{cfg: cfg, enabled: slotSet(cfg.EnableSlots)}
}

type defaultSystemAssembler struct {
	cfg     SystemAssemblerConfig
	enabled map[SystemSlot]bool

	once    sync.Once
	project []ProjectDoc
	skills  []Skill
	loadErr error
}

// hasSkillLoadEvent 判断会话日志里是否已有 skill/load 事件（每会话只写一次的幂等判据）。
func hasSkillLoadEvent(sess *session.Session) bool {
	if sess == nil {
		return false
	}
	for _, ev := range sess.Events {
		if ev.Type == event.SessionEventSkillLoad {
			return true
		}
	}
	return false
}

// Assemble 实现 SystemAssembler：按固定顺序输出启用的槽位分段。
// 项目指令与技能清单仅在首次调用时从磁盘加载（配置不变则缓存复用）。
func (a *defaultSystemAssembler) Assemble(ctx context.Context, sess *session.Session, tools []llm.Tool) ([]SystemSection, error) {
	a.once.Do(func() {
		a.project, a.loadErr = LoadProjectDocs(a.cfg.ProjectDocDir)
		if a.loadErr != nil {
			return
		}
		a.skills, a.loadErr = LoadSkills(a.cfg.SkillsDir)
	})

	var out []SystemSection
	add := func(slot SystemSlot, title, content string) {
		if !a.enabled[slot] || strings.TrimSpace(content) == "" {
			return
		}
		out = append(out, SystemSection{Title: title, Content: content})
	}

	add(SlotConstraints, "系统约束", a.cfg.Constraints)
	if len(a.project) > 0 {
		add(SlotProject, "项目指令", renderProjectDocs(a.project))
	}
	add(SlotTask, "任务定义", sess.State.Goal)
	add(SlotState, "当前状态", sess.State.Snapshot())
	if len(tools) > 0 {
		add(SlotTools, "工具签名", renderTools(tools))
	}
	if len(a.skills) > 0 {
		add(SlotSkills, "技能清单", renderSkills(a.skills))
		// 写 skill/load 事件（durable）：每会话一次，把全部 Skill frontmatter 摘要落
		// Session Event Log（与 approval/checkpoint 事件同等地位）。
		//
		// 幂等判据取自会话日志本身（而非 a.once）：本组装器由 buildRunContext 装配后
		// 被同一 Assistant 的所有会话共享，once 只会让"第一个跑到这里的会话"拿到事件，
		// 其余会话永久缺失。日志即事实——扫已有事件是唯一对多会话与 Resume 都正确的判据。
		// 事件落库失败向上抛（fail-fast），与 session.Restore 一致。
		if !hasSkillLoadEvent(sess) {
			metas := make([]event.SkillMeta, 0, len(a.skills))
			for _, s := range a.skills {
				metas = append(metas, event.SkillMeta{
					Name: s.Name, Description: s.Description,
					SchemaVersion: s.SchemaVersion, WhenToUse: s.WhenToUse,
					AllowedTools: s.AllowedTools, References: s.References,
					Source: s.Source, UserInvocable: s.UserInvocable,
				})
			}
			if err := sess.Append(event.SessionEventSkillLoad, event.MarshalData(event.SkillLoadData{Skills: metas})); err != nil {
				return out, fmt.Errorf("system assembler: append skill/load: %w", err)
			}
		}
	}
	// 历史摘要：默认关闭（Compactor 已注入消息历史），显式启用时取自 TaskState 快照
	add(SlotSummary, "历史摘要", sess.State.Snapshot())
	return out, nil
}

// RenderSystem 将分段渲染为系统提示词文本：`【标题】\n内容` 逐段拼接。
// 空内容分段被跳过；返回结果去除首尾空白。
func RenderSystem(sections []SystemSection) string {
	var b strings.Builder
	for _, s := range sections {
		content := strings.TrimSpace(s.Content)
		if content == "" {
			continue
		}
		if s.Title != "" {
			b.WriteString("【")
			b.WriteString(s.Title)
			b.WriteString("】\n")
		}
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// renderTools 将工具清单渲染为紧凑的逐行列表（name: description）。
func renderTools(tools []llm.Tool) string {
	var b strings.Builder
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(t.Name)
		if desc := strings.TrimSpace(t.Description); desc != "" {
			b.WriteString(": ")
			b.WriteString(desc)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// slotSet 将启用槽位列表转换为集合。
func slotSet(slots []SystemSlot) map[SystemSlot]bool {
	m := make(map[SystemSlot]bool, len(slots))
	for _, s := range slots {
		m[s] = true
	}
	return m
}
