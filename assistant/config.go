// Package assistant 提供组装好的智能体：模型对象、工具集与框架概念（策略/规划/审计/记忆/观察）
// 均由调用方注入，本包负责装配并提供默认实现，供上层应用（CLI、服务等）一行代码集成。
//
// 本包属框架层能力：只定义组装与运行的 API 契约（New / Run / Config）。
// 模型以对象形式注入（llm.ChatModel），工具以对象形式注入（[]tool.Tool），
// 本包不关心模型接入配置与具体工具实现（API Key / 地址 / 模型名如何构建属应用层事务，可参考 internal/config）。
package assistant

import (
	"hegq.net/mos/agent"
	"hegq.net/mos/memory"
	"hegq.net/mos/planner"
	"hegq.net/mos/tool"
)

// Config 是智能体的运行配置（框架层 API 契约）。
// 注意：模型与工具不在此配置中——分别通过 New 的 model 与 tools 参数注入。
// 框架级概念（Policy/Planner/Auditor/Memory/Observer）由外部注入实例；
// 为 nil 时装配层使用框架默认实现，可在 New 前按需覆盖。
type Config struct {
	// SystemPrompt 系统引导词。
	SystemPrompt string
	// MaxTurns 一次任务循环的最大 Turn 数（0 表示使用框架默认）。
	MaxTurns int

	// Policy 工具调用安全策略（默认 NewDefaultPolicy(nil, "")，即无额外限制）。
	Policy agent.Policy
	// Planner 决策规划器（默认 ReAct(model)）。
	Planner planner.Planner
	// Auditor 工具结果审计器（默认 agent.NewDefaultAuditor()）。
	Auditor agent.Auditor
	// Memory 会话记忆存储（默认 inmemory.New()，进程内实现）。
	Memory memory.Memory
	// Observer 任务循环事件回调（可观测工具调用与思考过程）。
	Observer agent.Observer
	// Sandbox shell 命令执行沙箱（M1.6 隔离层）：工作目录 jail + 资源限制 + 进程组超时。
	// 为 nil 时 shell 工具使用默认执行方式（10s 超时 + 64KB 截断）。
	Sandbox *tool.Sandbox
	// Truncator 工具结果截断器（M2.3 上下文工程）：对模型可见的工具结果做
	// 行数/字节上限瘦身，超限时截断并携带逃生通道提示（完整结果落盘 + read_file 回读）。
	// 为 nil 时启用框架默认实现（64KB / 2000 行，见 agent.NewDefaultTruncator）。
	Truncator agent.ResultTruncator
}
