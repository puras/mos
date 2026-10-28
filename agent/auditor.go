// Package agent 承载上下文工程（S6）：截断、压缩、审计门。
//
// 上下文是稀缺资源（架构不变式 4），三件套各管一段：
//
//   - Truncator（截断）：单次工具结果过大时就地瘦身，给模型的回填文本
//     不许撑爆上下文；完整输出可落盘留逃生通道。规则对齐 PI 的
//     tools/truncate.ts（行/字节双限、整行保留、head/tail/both）。
//   - Compactor（压缩）：超过 token 预算（contextWindow - reserveTokens）
//     时，从投影视图定切割点（永不落在 tool 结果上），由**模型**生成
//     结构化摘要（Goal/Progress/Key Decisions/Next Steps，PI 同款）；
//     摘要作为 compaction 事件落会话日志，投影从此遮蔽旧历史——被遮蔽
//     的内容不删，重放确定性地复现压缩后视图（PI CompactionEntry /
//     DSH marker 消息同哲学）。
//   - Auditor（审计门）：哪些工具的输出配得上"事实"二字，由它裁定——
//     只有环境自证的只读工具可入 TaskState，模型自述一律不算数
//     （审计不变式；PI/DSH 均无此件，属 MoS PRD 的 M2.1/M2.2 基线，
//     由 M3 多智能体与宿主消费）。
//
// 三者都是纯机制：白名单、窗口与预算、落盘目录是宿主的策略，经装配注入。
package agent

// Auditor 是审计门（Audit Gate）：裁定某工具的输出能否作为已验证事实
// 进入 TaskState。
//
// 只读/自证类工具（current_time、read）的输出本身就是环境的回答，
// 可信；变更类工具（shell、write）的输出属于"未核实声明"——它描述
// 的世界状态可能是这次变更造出来的假象，要进状态需先用只读工具二次取证。
// 宿主确信某变更类工具的输出可作证据时（如结构化的测试运行器），用
// Trust 显式扩名单——决策在宿主，机制在框架。
type Auditor interface {
	// Trusted 报告该工具的输出是否为环境自证（可写入任务状态）。
	Trusted(toolName string) bool
}

// DefaultAuditor 是基于只读白名单的默认审计器。
//
// Allow 为 nil 时用内置白名单（本仓库现有的只读工具）；非 nil 时**以它
// 为准**（空集 = 全不信任，fail-closed）——装配了自定义名单又意外为空，
// 后果应当是"没有事实进状态"，而不是"什么都被信任"。
type DefaultAuditor struct {
	// Allow 是信任的工具名集合；nil 表示用内置白名单。
	Allow map[string]bool
}

// 内置只读白名单：输出即环境回答的工具。S5d/S5f 之后本仓库的内置工具
// 就这几个；web_* 等随切片加入后在此登记。
//
// **web_search / web_fetch 故意不在白名单里**：它们的输出是**外部声明**
// （别人写的网页与搜索结果），不是环境自证——模型可以把"我认为是这样"写成
// 网页内容，审计门不能把它当事实。宿主要用它们的内容进 TaskState，得显式
// Trust（决策在宿主，机制在框架）。
var defaultTrusted = map[string]bool{
	"current_time": true,
	"read":         true,
	"grep":         true,
	"glob":         true,
	"ls":           true,
}

// NewDefaultAuditor 创建默认审计器（内置只读白名单）。
func NewDefaultAuditor() *DefaultAuditor { return &DefaultAuditor{} }

// Trust 把工具加入信任名单（合并到当前生效的名单上；内置白名单视为基线）。
func (d *DefaultAuditor) Trust(names ...string) {
	if d.Allow == nil {
		d.Allow = make(map[string]bool, len(defaultTrusted)+len(names))
		for name := range defaultTrusted {
			d.Allow[name] = true
		}
	}
	for _, name := range names {
		d.Allow[name] = true
	}
}

// Trusted 实现 Auditor。
func (d *DefaultAuditor) Trusted(toolName string) bool {
	if d == nil {
		return false
	}
	if d.Allow == nil {
		return defaultTrusted[toolName]
	}
	return d.Allow[toolName]
}
