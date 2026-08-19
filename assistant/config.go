// Package assistant 提供组装好的智能体：模型对象、工具集与框架概念（策略/规划/审计/记忆/观察）
// 均由调用方注入，本包负责装配并提供默认实现，供上层应用（CLI、服务等）一行代码集成。
//
// 本包属框架层能力：只定义组装与运行的 API 契约（New / Run / Config）。
// 模型以对象形式注入（llm.ChatModel），工具以对象形式注入（[]tool.Tool），
// 本包不关心模型接入配置与具体工具实现（API Key / 地址 / 模型名如何构建属应用层事务，可参考 internal/config）。
package assistant

import (
	"context"

	"hegq.net/mos/agent"
	"hegq.net/mos/agent/approval"
	"hegq.net/mos/agent/checkpoint"
	"hegq.net/mos/agent/driver"
	"hegq.net/mos/agent/event"
	"hegq.net/mos/agent/session"
	"hegq.net/mos/memory"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// Config 是智能体的运行配置（框架层 API 契约）。
// 注意：模型与工具不在此配置中——分别通过 New 的 model 与 tools 参数注入。
// 框架级概念（Policy/Planner/Auditor/Memory/Observer）由外部注入实例；
// 为 nil 时装配层使用框架默认实现，可在 New 前按需覆盖。
type Config struct {
	// Driver 驱动名（架构 v2 §3.5 按名选组件）：react（默认）/ plan-execute / reflection。
	// 自定义驱动名时使用 DriverFactory 注入。
	Driver string
	// DriverFactory 自定义驱动工厂（优先于 Driver 名；nil 时按 Driver 名内置装配）。
	DriverFactory func(rt *driver.RunContext) *driver.Agent
	// PlanPlanner Plan&Execute 计划生成器（Driver=plan-execute 时使用；nil 单步退化）。
	PlanPlanner planner.PlanPlanner
	// Reflector Reflection 评审器（Driver=reflection 时必需；nil 拒绝运行）。
	Reflector planner.Reflector

	// SystemPrompt 系统引导词（系统约束槽位内容）。
	// 设置了 Assembler 时，本字段作为默认组装器的约束槽位（可被组装器配置覆盖）。
	SystemPrompt string
	// MaxTurns 可选保险丝：一次任务循环的最大 Turn 数，>0 时达到上限仍未完成则阻塞；
	// 0 表示不限制（默认，对齐 DSH/PI 无轮次上限，循环以模型直接作答 /
	// ShouldStopAfterTurn 终止）。
	MaxTurns int
	// ShouldStopAfterTurn 每轮结束后的外部停止裁决（对齐 PI shouldStopAfterTurn）：
	// 返回 true 时任务以 completed 收尾；nil 时按默认行为（模型直接作答即完成）。
	ShouldStopAfterTurn func(ctx context.Context, sess *session.Session, turn int, action *planner.Action) (bool, error)
	// Assembler 槽位式系统提示词组装器（M2.6 动态组装）。
	// 设置后按槽位（系统约束/任务定义/当前状态/工具签名/项目指令/技能清单）每轮动态
	// 组装系统提示词；为 nil 时使用 SystemPrompt 静态引导。
	Assembler agent.SystemAssembler
	// ProjectDocDir 项目指令目录（M2.6）：从该目录向上递归查找 CLAUDE.md 合并，
	// 顶层在前、就近覆盖。与 Assembler 均为 nil 时无效；设置后将自动构建默认组装器。
	ProjectDocDir string
	// SkillsDir 技能目录（M2.6）：扫描其中的 SKILL.md（frontmatter 的 name/description）
	// 生成懒加载清单注入系统提示词，正文由模型触发时经 read_file 按需读取。
	// 与 Assembler 均为 nil 时无效；设置后将自动构建默认组装器。
	SkillsDir string

	// Policy 工具调用安全策略（默认 NewDefaultPolicy(nil, "")，即无额外限制）。
	Policy driver.Policy
	// Planner 决策规划器（默认 ReAct(model)）。
	Planner planner.Planner
	// Auditor 工具结果审计器（默认 agent.NewDefaultAuditor()）。
	Auditor agent.Auditor
	// Memory 会话记忆存储（v2 预留：会话历史以 Session 事件日志为唯一事实来源，
	// Memory 作为可选外部记忆后端，将在 SeamRegistry 迭代接入；当前配置不生效）。
	Memory memory.Memory
	// Retriever 长期记忆检索器（RAG 扩展点）：每轮按任务目标检索相关知识，
	// 经知识守门后注入上下文（携带来源引用）。为 nil 时不注入。
	Retriever memory.Retriever
	// RetrieverMinScore 知识守门最低分（Agentic RAG）：>0 时低于该阈值的
	// 检索分块不注入，全部低于阈值则判定知识差距（模型经外部工具补齐）。
	RetrieverMinScore float64
	// Observer 任务循环事件回调（可观测工具调用与思考过程）。
	Observer agent.Observer
	// Sandbox shell 命令执行沙箱（M1.6 隔离层）：工作目录 jail + 资源限制 + 进程组超时。
	// 为 nil 时 shell 工具使用默认执行方式（10s 超时 + 64KB 截断）。
	Sandbox *tool.Sandbox
	// FileAccess 文件工具访问策略（M3.x — file-tool-jail plan）：
	// 由 assistant.New 校验 WorkspaceRoot 必填（fail-closed）。
	// 透传给 tool/fs seam 装配层（Task 14），构造 Sandboxed/Local 与文件策略。
	FileAccess FileAccessConfig
	// Truncator 工具结果截断器（M2.3 上下文工程）：对模型可见的工具结果做
	// 行数/字节上限瘦身，超限时截断并携带逃生通道提示（完整结果落盘 + read_file 回读）。
	// 为 nil 时启用框架默认实现（64KB / 2000 行，见 agent.NewDefaultTruncator）。
	Truncator agent.ResultTruncator
	// Compactor fresh-context 压缩器（M2.5 上下文工程）：两轮之间历史超过阈值时，
	// 将旧消息压缩为摘要（TaskState 已验证事实）注入并保留最近消息，防跨轮上下文腐化。
	// 为 nil 时启用框架默认实现（保留最近 12 条，见 agent.NewDefaultCompactor）。
	Compactor agent.Compactor
	// ContextBudget 模型上下文窗口 token 预算（M2.5 上下文利用率监控）：
	// 每次模型调用后统计 PromptTokens/预算 利用率并广播 EventContextUsage 事件。
	// 为 0 时不监控。
	ContextBudget int
	// DoomLoopMaxRepeat Doom Loop 检测（M2.5）：相同工具连续 maxRepeat 次相同参数
	// 调用时判定为循环，任务中断（状态 blocked）。为 0 时不检测。
	DoomLoopMaxRepeat int

	// ---- M2.8 异步审批 / Checkpoint / 持久化 ----

	// Approval 异步审批通道（架构 v2 §3.4）：策略返回 *approval.RequiredError 后
	// 由驱动层发起 Approval.Request → 挂起（SuspendError）→ 平台 Resolve → Resume 续跑。
	// nil 时对需要确认的操作 fail-closed 拒绝。旧同步确认器经 approval.NewConfirmerAdapter
	// 桥接为 Approval。
	Approval approval.Approval
	// Confirm 旧同步确认器（Deprecated: M2.8 起使用 Approval；本字段设置后自动经
	// approval.NewConfirmerAdapter 桥接为 Approval，保留 CLI y/N 同步体验）。
	Confirm Confirmer
	// CheckpointStore checkpoint 存储（架构 v2 §3.8，参考 LangChain checkpointer）。
	// nil 时挂起点不落盘（SuspendError 仍冒泡，但无法跨进程恢复）。
	CheckpointStore checkpoint.Store
	// Persistence 会话事件日志后端（架构 v2 §10.3，backend 可换）。nil 时会话仅内存。
	// 配置后 NewSession 自动挂载，事件同步落盘（日志即事实）。
	Persistence session.Persistence
	// Bus 事件总线（Agent 域事件广播：agent/created 等）。nil 时装配层创建默认 EventBus
	// （修复 RunContext.Bus 始终为 nil 的旧缺陷，Notify 经此广播）。
	Bus *event.EventBus
}

// FileAccessConfig 描述文件工具的访问策略。
// WorkspaceRoot 必填；空值由 assistant.New 校验失败（fail-closed，Task 10）。
type FileAccessConfig struct {
	WorkspaceRoot string   // 文件系统 jail 根目录
	JailReads     bool     // 是否对读做 jail；DSH SaaS/read-only = true
	JailWrites    bool     // 是否对写做 jail；DSH 默认 = true
	WritableExtra []string // 附加可写根（如构建产物目录）
}
