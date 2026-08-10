# MoS

MoS stands for Sapient, representing a wisdom‑minded agent base.
**MoS 取自 Sapient，寓意面向心智智慧的智能体底座。**

MoS 是一个基于 Go 的智能体基础框架（Agent Base），提供统一的 LLM 接入、工具（函数调用）系统、会话记忆与可插拔的决策规划，帮助快速构建智能体应用。

## 特性

- **多协议 LLM 接入**：统一 `ChatModel` 抽象（模型对象）+ 协议注册表，内置 OpenAI 兼容（DeepSeek / 通义千问 / GLM / Ollama 等）与 Anthropic Claude 原生协议适配器，支持函数调用与流式输出
- **声明式工具系统**：结构体 tag 自动生成 JSON Schema，模型函数调用参数自动校验与解析
- **可插拔规划**：默认 ReAct 循环，可通过 `planner.Planner` 接口替换为其他策略
- **会话记忆**：统一的 `memory.Memory` 接口，内置内存实现；长期向量记忆（sqlite-vec）规划中
- **事件观测**：`Observer` 回调暴露工具调用与完成事件，便于流式输出与可观测性扩展
- **Harness Engineering（工具安全防护）**：`agent.Policy` 在每次工具调用执行前拦截检查，内置默认策略可拦截危险 Shell 命令、路径越界与非 http 抓取
- **Loop Engineering（循环可观测）**：每轮循环暴露思考/工具调用/工具结果/完成事件（含迭代序号），完整还原推理过程
- **审计式任务状态（Audit Gate + Task State）**：工具结果经 `Auditor` 审计后，仅"环境可独立验证"的事实写入独立于消息历史的 `TaskState`（目标 / 已验证事实 / 状态机）；模型自述不算数——执行上下文可刷新，进度真相不过期
- **开箱即用的智能体**：`assistant` 包组装好 Provider + 内置/扩展工具包 + 安全策略；交互式 CLI 见示例工程 `examples/assistant`
- **零第三方依赖**：SDK 仅使用 Go 标准库（示例工程除 SDK 外同样零依赖）

## 快速开始

```go
// 0. 创建模型对象（实现 llm.ChatModel 即可，见下方"LLM 接入"）
model := ... 

// 1. 注册工具：结构体定义参数，tag 自动生成 JSON Schema
type calcParams struct {
	A int `json:"a" mos:"desc=加数"`
	B int `json:"b" mos:"desc=被加数"`
}

type calculator struct{}

func (calculator) Name() string        { return "calculator" }
func (calculator) Description() string { return "计算两个整数之和" }
func (calculator) Parameters() any     { return &calcParams{} }
func (calculator) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[calcParams](args)
	if err != nil {
		return nil, err
	}
	return p.A + p.B, nil
}

reg := tool.NewRegistry()
reg.Register(calculator{})

// 2. 创建 Agent（ReAct 循环 + 事件观测）
a := agent.New(model, reg,
	agent.WithSystemPrompt("你是助手"),
	agent.WithObserver(func(ev agent.Event) { /* 观测工具调用/完成 */ }),
)

// 3. 执行任务（多轮对话自动保留历史）
sess := agent.NewSession("demo")
reply, err := a.Run(ctx, sess, "请计算 1+2")
```

## 开箱即用的智能体（assistant + CLI 示例）

`assistant` 包提供组装好的智能体：**模型对象、工具集与框架概念全部由调用方注入**（安全策略 / 规划器 / 审计器 / 记忆 / 观察，为 nil 时使用框架默认实现），可直接作为库集成到其他工程。框架只认 `llm.ChatModel` 模型对象与 `[]tool.Tool` 工具集，不预设默认工具、不关心模型接入配置——但**随框架分发工具包并分类**：`pkg/tool/builtin` 内置工具包（计算 / 时间 / 文件读写检索 / 网页抓取，纯标准库实现，`builtin.Default()` 一键装配）+ `pkg/tool/extension` 扩展工具包（shell 沙箱执行 / web_search 搜索提供商，需依赖注入）：

```go
// 1. 构建模型对象（可用 openai/anthropic 适配器，或任何自定义 llm.ChatModel）
model, err := openai.New(llm.Config{
	"api_key": os.Getenv("OPENAI_API_KEY"),
	"model":   "gpt-4o",
})
if err != nil {
	// ...
}

// 2. 注入模型对象 + 工具集 + 运行配置（概念对象由业务侧构造注入）
a, err := assistant.New(model, []tool.Tool{
	builtin.Calculator{}, builtin.ReadFile{}, builtin.WriteFile{}, // 内置工具包按需取用
}, assistant.Config{
	SystemPrompt: "你是 MoS 智能体",
	Policy:       assistant.NewDefaultPolicy([]string{"ls", "cat", "pwd"}, "./workspace"), // 安全策略
})
// ...
sess := a.NewSession()
reply, err := a.Run(ctx, sess, "帮我看看 workspace 里有什么文件")
```

运行配置可通过环境变量加载；模型对象从环境变量构建（`examples/assistant/internal/config` 的应用层实现，无 API Key 时自动降级为 mock 离线模型对象 `assistant.Mock()`）：

| 环境变量 | 说明 |
| --- | --- |
| `MOS_PROVIDER` | openai / anthropic / mock（默认 openai，无 Key 自动降级 mock） |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | 对应协议的密钥 |
| `MOS_BASE_URL` | 自定义网关地址（兼容 OpenAI 协议） |
| `MOS_MODEL` | 模型名 |
| `MOS_SYSTEM_PROMPT` | 系统引导词 |
| `MOS_MAX_TURNS` | 一次任务循环的最大 Turn 数（默认 10，达到上限仍未完成则阻塞） |
| `MOS_TOOLS` | 按需启用的工具名，逗号分隔（空表示全部） |
| `MOS_SHELL_ALLOW` | Shell 命令白名单，逗号分隔 |
| `MOS_WORKDIR` | 文件类工具与受限危险命令的工作目录 |
| `MOS_SANDBOX` | 执行沙箱总开关（默认 1 开启；0 关闭则 shell 用默认执行方式） |
| `MOS_SANDBOX_TIMEOUT` | 沙箱单次命令超时秒数（默认 10） |
| `MOS_SANDBOX_MAX_OUTPUT` | 沙箱输出上限 KB（默认 64，超出截断丢弃） |
| `MOS_SANDBOX_CPU_SECONDS` | 沙箱 CPU 时间上限秒数（0 不限制） |
| `MOS_SANDBOX_MEM_KB` | 沙箱虚拟内存上限 KB（0 不限制） |
| `MOS_SANDBOX_MAX_FILE_KB` | 沙箱单文件大小上限 KB（0 不限制） |
| `MOS_SANDBOX_NO_NET` | 沙箱网络隔离（1 开启，尽力而为：剥离代理变量） |

以上配置也可写入 **`.env` 文件**（默认 `examples/assistant/.env`，路径可用 `MOS_CONFIG` 覆盖）。键名与环境变量完全一致，读取优先级：**环境变量 > .env 文件 > 默认值**（`.env` 不覆盖已存在的环境变量）：

```
MOS_PROVIDER=openai
OPENAI_API_KEY=sk-xxx
MOS_BASE_URL=https://api.deepseek.com/v1
MOS_MODEL=deepseek-chat
MOS_SYSTEM_PROMPT=你是 MoS 智能体
MOS_MAX_TURNS=10
MOS_SHELL_ALLOW=ls, cat, pwd
MOS_WORKDIR=./workspace
```

支持 `#` 注释、空行与可选的 `export` 前缀；`OPENAI_API_KEY` / `ANTHROPIC_API_KEY` 均为空时自动降级为 mock 离线模型。模板见 `examples/assistant/.env.example`。

工具包分两类（均在 `pkg/tool` 下随框架分发）：**内置工具包 `pkg/tool/builtin`**（纯标准库，`builtin.Default()` 一键装配）——`calculator`（表达式求值）、`current_time`（当前时间）、`web_fetch`（网页抓取转纯文本）、`read_file` / `write_file`（文件读写）、`find_files` / `grep`（文件搜索）；**扩展工具包 `pkg/tool/extension`**（需依赖注入）——`shell`（受限命令执行，支持沙箱隔离）、`web_search`（联网搜索，绑定搜索提供商，提供商实现见 `pkg/tool/extension/search`）。

交互式 CLI 在示例工程 `examples/assistant` 中（同样支持环境变量配置，无 Key 时以 mock 演示完整工具调用循环）：

```bash
cd examples/assistant && go run .
```

```
你 > 计算 (1+2)*3
  ⟳ [第1轮思考] 让我来处理
  ▶ [第1轮] 调用工具 calculator({"expr":"(1+2)*3"})
  ◀ [第1轮] 工具结果: 9
  ▶ [第2轮] 调用工具 current_time({})
  ⟳ [第3轮思考] 计算完成，结果为 9。...
智能体 > 计算完成，结果为 9。...
```

### Harness Engineering：工具安全防护

`agent.Policy` 接口在每次工具调用**执行前**拦截检查，返回错误即拒绝该次调用，并将拒绝原因回填给模型：

```go
type Policy interface {
	Allow(ctx context.Context, call *llm.ToolCall) error
}
```

默认策略 `assistant.NewDefaultPolicy(shellAllowed []string, workDir string)` 提供分级防护（**fail-closed**：不在放行名单的未知工具一律拒绝，新增工具须在 `Allow` 中显式声明放行）：

- **绝对危险命令**（shutdown / reboot / halt / poweroff / mkfs / dd / 写块设备 / fork 炸弹）：无条件拒绝，不提供确认放行
- **受限危险命令**（rm / sudo / chmod / chown / mv / kill / 管道 shell / **解释器执行入口** `bash -c`、`python3 -c`、`perl -e`、`node -e`、`ruby -e`、`php -r` 等 / **find -delete / -exec**）：路径必须落在 `WorkDir` 内，且须经 `Confirmer` 人工确认后才执行；未配置确认器、确认拒绝、或未配置 `WorkDir` 时一律拒绝——"范围有限 + 操作前确认"（如创建文件后删除临时文件，限定在 `WorkDir` 内的 `rm` 经确认后可用）。覆盖解释器执行入口是为了防止模型绕过黑名单：`rm` 被拒后改用 `python3 -c "os.remove(...)"` / `bash -c "rm -rf ..."` / `find . -delete` 达成删除意图
- **命令前缀白名单**：配置 `ShellAllowed` 后仅允许白名单前缀的命令
- **路径越界防护**：基于 `filepath.Rel` 校验文件/目录操作必须落在 `WorkDir` 内；web_fetch 仅允许 http/https；web_search 校验查询参数
- **只读/自证工具**（calculator / current_time / read_file / find_files / grep / web_fetch / web_search）显式放行；`write_file` 路径受限（配置了 `Confirmer` 时写入前再确认）

危险操作确认器 `assistant.Confirmer`（M2.8 人工介入的轻量雏形）：CLI 纯文本模式注入交互确认（y/N）；全屏 TUI 不注入（stdin 被界面接管，受限危险操作按 fail-closed 拒绝）；库集成方可注入自定义确认器。

以上是**规则层**（执行前拦截）；纵深防御还需要**隔离层**（执行中限制）——`tool.Sandbox` 抽象：Policy 在沙箱外拦截"该不该调用"，工具在沙箱内执行"放行后能造成多大破坏"。本地轻量实现（M1.6）为进程资源限制（CPU / 虚拟内存 / 单文件）+ 工作目录 jail + 单次调用超时 + 输出截断 + 环境裁剪 + 网络控制（NoNet 剥离代理，尽力而为）；远程强隔离（M4）可换 Docker / containerd 后端，对外 API 不变。

**诚实的边界**：黑名单是"命令形态"约束，无法穷尽删除/破坏意图的表达式（解释器、`find -delete`、混淆命令都可能绕过）；模型被拒后还会主动寻找替代工具达成原意图。规则层只能提高攻击成本，**真正的安全边界是沙箱隔离**——在 jail 内即使 `python3 os.remove()` 成功，破坏面也被限制在 `WorkDir` 内。即便 M1.6 已落地，本地沙箱也只是纵深防御的一层（进程级隔离，非虚拟化），完整安全保证仍依赖远程强隔离后端（M4）。

### Loop Engineering：循环可观测

每轮循环依次发出 `EventStep`（思考）→ `EventToolCall` → `EventToolResult` → `EventFact`（审计通过，任务状态新增事实）→（循环）→ `EventDone`（完成），事件携带 `Turn` 序号，方便流式输出与过程审计。

业界 Loop Engineering（agent-harness-kit / IBM）把"循环"本身当作一等设计对象：循环不是模型自由发挥的黑盒，而是由一组杠杆（lever）显式控制的工程构件。MoS 对照九杠杆：

| 杠杆 | 业界要点 | MoS 现状 |
| --- | --- | --- |
| 确定性传感器 | 用测试 / typecheck / lint 等可验证信号代替"问模型对不对" | ✓ 审计门 `Auditor` + Audit Gate（只接受环境验证过的事实） |
| 显式停止条件 | 可验证的 DONE 信号 + 硬迭代上限 / 预算上限 | ✓ 完成判断 + `MaxTurns`；token 预算上限待 M4 |
| 跨迭代上下文管理 | 压缩旧轮、JIT 检索、手写 scratchpad，防 context rot | ~ 截断（M2.3）/ 摘要（M2.5）/ 记忆（M2.7）规划中 |
| 清晰工具设计 | 单次调用单意图、返回 crisp signal，不留歧义 | ✓ 内置 / 扩展工具签名统一；输出截断（M2.3）保留关键字段 |
| Doom Loop 检测 | 相同工具连续 3+ 次相似调用 → 死循环检测 / 回退 / 重置 | ✗ 待新增（随 M2.5） |
| 上下文利用率阈值 | Dumb Zone：窗口用满约 40% 后输出质量明显下降 | ✗ 待新增 40% 告警线 + 渐进式披露（随 M2.5） |
| 进度追踪 | 维护"做到哪了"而非完整对话记录 | ✓ `TaskState` 外置、跨轮唯一真相 |
| 可观测性 | 事件流 + trace 回放、过程审计 | ✓ Observer / Event（Turn 序号） |
| 补偿面理论 | 模型变强 → harness 主动拆组件（Anthropic 已拆 Context Reset），只留护城河 | ✗ 设计纪律（见路线图方向） |

两个关键阈值：**上下文利用率约 40%**（168K 窗口）之后模型输出质量明显下降——应对是渐进式披露（先给骨架、按需展开）、分层上下文、40% 告警线、必要时上下文重置（fresh-context，M2.5）；**Doom Loop**（相同工具连续 3 次以上相似动作）说明模型在空转——检测到后应回退到上一有效状态或重置上下文，而不是让循环耗尽预算。补偿面理论提示：这些杠杆不是越多越好，模型变强后 harness 应主动拆掉已被模型自身能力覆盖的组件（如 Anthropic 拆掉 Context Reset 改由模型记忆承担），把工程资源集中在模型替代不了的护城河上（沙箱、审计、状态外置）。

### Graph Engineering：显式图编排

Graph Engineering（LangGraph 为代表的"图编排"）把 Loop 的单链循环升级为显式图。四个原语：

- **节点（Node）**：有界执行单元——业务规则宜用确定性函数而非 LLM；节点可承载 LLM 调用、工具 Agent、纯函数、检索、策略检查、人工审批或子图
- **边（Edge）**：流转定义——直接 / 条件 / 并行 / 循环 / 错误 / 人工控制 / 事件触发
- **状态（State）**：共享记录（TypedDict schema；reducers 如 `Annotated[list, add]` 让并行节点合并而非覆盖），使输入输出可见、免去传递完整对话记录
- **Checkpointer**：持久化状态，支持中断恢复（SqliteSaver / PostgresSaver 参考）

MoS 现架构已内嵌图概念，Graph Engineering 是显式化而非重构：

- **TaskState = State**：跨迭代持久状态已是图的共享记录；审计写入即 reducer（只合并已验证事实，并行节点互不覆盖）
- **Auditor = 确定性传感器**：节点边界验证，对应"业务规则宜确定性非 LLM"
- **Policy + Sandbox + Confirmer = 守卫节点**：工具调用前的约束校验与人工审批
- **Observer = 边事件**：hooks 挂载点，即边的触发与观测
- **生命周期命名 = 图词汇**：`Thread` 已是"DAG 节点路径"，`Session` 可压缩可分叉

图编排的使用判据（LangGraph 的 earns-its-keep）：**分支**（Branching）、**并行**（Parallelism）、**持久性**（Durability，崩溃后可恢复）、**可审计性**（Auditability，逐步解释）。MoS 的单任务顺序循环不满足判据时保持现状；满足时（M3 多智能体、LLMCompiler 并行 DAG）才引入显式图边界——图是可选编排层，不是默认架构。

### Audit Gate：审计式任务状态

对应长程 Agent 的"审计式状态迁移"：`agent.Auditor` 在每次工具调用后独立审查结果，只有**被环境验证过的事实**才能写入 `Session.State`（`agent.TaskState`），模型的自我声明一律不算数。默认审计器 `agent.NewDefaultAuditor()` 采用只读白名单——`calculator` / `current_time` / `web_fetch` / `web_search` / `read_file` / `find_files` / `grep` 的输出本身就是环境的回答，视为已验证事实；`shell` / `write_file` 等变更类工具的输出属于"未核实声明"，不写入任务状态，需后续通过只读工具二次取证。可自定义 `Auditor` 或经 `Trust` 扩展白名单：

```go
a := agent.New(model, reg,
	agent.WithAuditor(agent.NewDefaultAuditor()),
)
// 自定义只读工具也产出可验证事实
aud := agent.NewDefaultAuditor()
aud.Trust("my_inspect")
```

任务状态独立于消息历史存在：历史可丢失、可压缩、可刷新，但"任务做到哪了"这个真相始终可恢复（跨轮次唯一记忆 = 审计报告）。

### 生命周期与命名规范

智能体整体生命周期的概念层级（从粗到细）与统一命名：

```
┌─ Run（一次运行）─────────────────────────────┐
│  agent_start → agent_end                    │
│  ├─ Turn 1: 模型调用 → 工具执行(批) → 审计 → 状态 │
│  ├─ Turn 2: 模型调用 → 工具执行(批) → 审计 → 状态 │
│  └─ Turn N: 模型调用 → 无工具调用 → 完成       │
└─────────────────────────────────────────────┘
  Session（跨 Run 累积 History + TaskState；可压缩、可分叉）
  Thread（Session 的分支，DAG 节点路径）
  Task（用户目标 + 状态机，贯穿 Session 生命周期）
```

| 概念 | 定义 | 生命周期边界 | 对应代码 |
| --- | --- | --- | --- |
| **Run** | 一次完整执行：接收输入 → 产出回复 / 阻塞退出 | `agent_start` → `agent_end` | `Agent.Run` |
| **Turn** | 一次模型调用 + 该次调用触发的所有工具执行 | `turn_start` → `turn_end` | 事件 `Event.Turn` 序号 |
| **Session** | 跨 Run 的对话单元：消息历史 + 可信任务状态 | 会话创建 → 关闭/分叉 | `agent.Session` |
| **Thread** | 从历史某点分叉出的会话分支（DAG 路径） | 分支创建 → 合并/废弃 | 预留（M2.7） |
| **Task** | 用户目标 + 状态机（pending/running/blocked/completed） | 首个输入 → 完成/阻塞 | `agent.TaskState` |
| **Round** | 子任务轮次：Manager 派发 → Executor 执行 → Auditor 审计 | 多智能体启用（M3） | 预留 |
| **Team** | 多智能体团队：Team Lead + Teammates + Shared Task List | 团队创建 → 解散 / 恢复 | 预留（M3） |
| **Message** | 消息：3 种标准（user/assistant/tool）+ 自定义（摘要等） | 单次交互 | `llm.Message` |

生命周期规则（约定俗成的工程决策，实现时遵循）：

1. **循环终止**：模型输出无工具调用即视为完成——"不是模型说完成了，是它没要工具就当你完成了"；任一工具 `terminate` → 停止；`error/aborted` → 硬停止，且错误注入为消息反馈而非抛异常中断。
2. **Turn 边界**：一个 Turn 至多一次模型调用；工具以批执行（同一 Turn 内的多个工具调用一次跑完）。
3. **状态写入**：仅 Auditor 验证通过的事实进 `TaskState.Facts`；模型自述不写入（环境说了算）。
4. **压缩**：两轮之间触发；切割点不得切断 ToolCall-Result 对（不切在 toolResult 后）；摘要 = `TaskState.Snapshot()` + 最近消息（审计写入的无损记忆，而非 LLM 重新总结）。
5. **状态机**：`pending → running → completed/blocked`，跨 Run 保持。
6. **消息角色序**：user/assistant 交替；自定义消息在翻译边界（M2.4 的 convertToLlm）统一转 user 角色。

**设计取舍（状态在上下文 vs 状态外置）**：短任务（几轮内）可直接把状态写进上下文，只需 ReAct 无需外置状态，最省事；长程任务必须外置（`TaskState`），否则错误复合/上下文腐化不可避免。MoS 以状态外置为主、默认开启审计门，短任务场景可关闭审计门退化为纯 ReAct。

**主观自查 vs 客观验证**：`Auditor` 是环境客观验证（事实才入状态）；Reflection（Generator → Reflector → Revisor）是模型主观自查（提升输出质量、规避盲区）。两者互补：自查发现的问题仍需 Auditor 验证后才能成为事实。

**Agent 设计模式参考（Agentic Design Patterns）**：与 MoS 直接相关的三个模式——
- **路由（Routing）**：4 种机制（LLM 路由 / 嵌入路由 / 规则路由 / ML 分类器路由）；生效节点：任务入口（意图分类）、流程中间（分支）、子程序（工具选择）。M3 的 Manager 即 Coordinator 路由——按意图把任务分派给专门子智能体，确定性强的用规则路由（可测试、可审计），模糊的用 LLM 路由。
- **异常处理与恢复（Exception Handling & Recovery）**：三阶段——错误检测（工具输出验证、API 错误码、超时）→ 错误处理（日志 / 重试 / 回退 / 优雅降级 / 通知）→ 恢复（状态回滚 / 诊断 / 自我纠正 / 升级）。检测层与 Auditor 重合（工具输出验证）；恢复层的"升级"直通 M2.8 人工介入。
- **人机协同（HITL）**：人类监督 / 干预纠正 / 反馈学习 / 决策增强 / 协作 / 升级策略——升级策略定义"何时、如何交给人类"；变体 Human-on-the-loop（人定策略、AI 自动执行）正是 MoS 已有的 `agent.Policy` 形态。权衡：可扩展性不足、依赖专家、敏感数据需匿名化。

**Harness Engineering（驾驭工程，Agent = Model + Harness）**：四条反直觉铁律，与 MoS 现有设计互证——
- **上下文越少越好**：上下文是稀缺资源会被污染。对应 M2.3 / M2.5（截断 + 压缩）；上下文按"系统约束 / 任务定义 / 当前状态 / 工具签名 / 历史摘要"分槽位（M2.6）。
- **专才 > 通才**：通才 Agent 在工具列表里"逛超市"。对应 Sub-Agent 隔离——独立 Context、只装载自己需要的工具、主 Agent 只收结构化输出（M3）。
- **状态写文件不写上下文**：Context 是易失存储、Workspace 是持久内存（"Agent 的 Git 仓库"）。对应 M2.1 状态外置 + M4 `runs/<run-id>` 可恢复目录；事务文件支持断点续传。
- **能写成 Linter 的约束别写成文档**：自然语言可被模型"创造性解读"，代码不能。对应 MoS 的 `agent.Policy`；且 **Linter 错误信息本身就是上下文工程**——报错要解释"为何有此规则、正确做法是什么"，让 Agent 读到后自我修正（M3 错误注入反馈）。

六大工程模式其余参考：**双阶段架构**（Initializer 理解任务→写 plan.md→退出；Executor 读 plan.md 接力，跨 Context Window，即 Plan&Solve）、**工具签名即文档**（动词短语命名 + 参数"何时用/何时别用"描述 + 稳定返回结构，改善选工具准确率）、**智能体审智能体**（Reviewer 只看 diff + 规则文件、对产出一无所知，避免同一 Context 复现同一偏见——Basic Reflection 的设计要点）、**熵管理**（后台文档园丁定期清理，M4 可选）。

**九种编排模式谱系（知乎《Agent 的九种设计模式》）**：ReAct（思考-行动-观察，MoS 默认循环）→ Plan&Solve（Planner / Replanner + Executor，计划随新约束修订）→ REWOO（Planner 生成链式依赖计划 → Worker 循环执行 → Solver 整合，Observation 隐式嵌于下一步）→ Storm（大纲生成器 + 内容生成器）→ LLMCompiler（Planner 生成 DAG + Joiner 合并，并行 function calling）→ Basic Reflection（Generator / Reflector）→ Reflexion（引入外部数据评估后 Revisor 修订，比 Basic Reflection 更具建设性）→ LATS（树搜索 + ReAct + Plan&Solve + Reflection + RL）→ Self-Discover（Selector 选推理方式 → Adaptor 应用 → Implementor 重推理）。M3 已规划 Plan&Solve / Basic Reflection / LLMCompiler；Reflexion 的"外部数据评估"恰与 Auditor 客观验证同构。

**编排拓扑与共享本体（ADP 多智能体协作 / 并行化）**：协作形式七种（顺序交接 / 并行 / 辩论共识 / 层次 / 专家团队 / 批评者-审查者 / 混合），通信结构六种（单智能体 / 网络 / 监督者 / 监督者作工具 / 层次化 / 自定义）——M3 先落地"监督者/层次化"（与 Planner + 执行器契合），再演进"批评者-审查者"（即审计智能体）。并行化采用 Map-Reduce 形态：无依赖分支并发、结果经共享状态（TaskState 分支字段）汇聚、合并步骤只读分支摘要禁止引入外部知识、并行度作为计划元数据（`parallelizable`）。**TaskState 即共享本体**——各智能体读写同一状态交换数据，需定义字段级契约（谁写、谁读、格式）。

**SubAgent（子智能体，Claude Code / OpenAI Agents SDK 参考）**：把任务委托给专用智能体执行，主智能体保持调度。业界两种调用形态（OpenAI 编排指南）：
- **委托执行（handoff 语义）**：控制权交给专家、由专家负责该分支的最终答复——对应 M3 Coordinator 路由按意图分派
- **代理即工具（agents-as-tools）**：主智能体保持所有权，把 SubAgent 包装为普通工具经 function calling 调用、只接收结构化结果——与 MoS 现有工具系统（`Registry` + `tool.Tool`）无缝对接，是 MoS 首选形态

SubAgent 声明式定义（对齐 Claude Code 的 `.claude/agents/*.md` frontmatter 与 SKILL.md 风格）：`name`（标识符）+ `description`（**自动委派的判断材料**——主智能体按 description 匹配最合适的专家）+ `model`（可选覆盖，快任务用小模型降本）+ `tools`（**工具白名单**）+ 系统提示词正文。安全关键：**白名单即 blast radius 收缩**——doc-reviewer 只给 Read / Grep / Glob 就杜绝误写文件；每个 SubAgent 的 `tools` 白名单即其 `agent.Policy` 的 Allow 名单，fail-closed 语义不变。上下文隔离：SubAgent 以"空白"状态启动，只接收任务描述（结构化输入）、只回传结构化摘要，中间工具调用留在内部——主会话上下文不膨胀。审计边界：SubAgent 回传的自我声明不算数，仍须经 `Auditor` 环境验证后才写入 TaskState。生命周期：任务完成即结束（短暂），对应 Round 概念；多个独立子任务可并行执行（Map-Reduce）。**决策判据（"合同"论）**：只有指令 / 工具 / 策略（instructions/tools/policy）真正改变才值得拆 SubAgent；单纯为并行而拆会制造更多 prompt、trace 与审批面。

**AgentTeam（智能体团队，Claude Code Agent Teams / ATWZ 参考）**：多个持续可用的智能体组成团队，由 Team Lead 领导、通过共享任务列表自我协调。四大组件：
- **Team Lead**：拆解任务、创建队友、协调进度、汇总结果（主会话）
- **Teammates**：各自独立执行分配任务（独立 Agent 实例 / 独立上下文）
- **Shared Task List**：共享任务队列——`pending → in_progress → completed` + `blocks / blockedBy` 依赖 + 自动解锁（依赖完成即变为可领取）；与 TaskState 字段级契约一致（谁写、谁读、格式）
- **Mailbox**：队友间点对点消息队列（文件即消息，对齐 ATWZ）

与 SubAgent 的本质区别：**通信**（SubAgent 只向主智能体汇报 vs 队友间可直接通信）、**协调**（主智能体统一管理 vs 共享任务列表自我协调）、**生命周期**（任务完成即结束 vs 队友持续可用、可跨会话恢复）、**信息可见性**（主智能体只见最终摘要 vs 实时消息、进度透明）、**成本**（较低 vs 每队友独立实例较高）。适用场景：聚焦任务只需结果 → SubAgent；复杂任务需要讨论协作、跨会话持续 → AgentTeam。

MoS 的 AgentTeam 关键设计：**状态落盘（files over context）**——对齐 M2.1 状态外置 + M4 `runs/<run-id>` 可恢复目录：队友是短暂的（ephemeral），角色 / 工作上下文 / 决策 / 任务文件 / 团队名册全部写盘（ATWZ 论文针对"不可恢复团队 + compaction 侵蚀细节 + agentic 技术债"三痛点）；中断后一条命令重建整个团队。**工作树隔离**——每个队友在独立工作目录 / 分支执行，测试通过才合并（对齐 M1.6 沙箱 WorkDir jail 思想扩展到团队，防文件冲突）。**审计不变式**：无论 SubAgent 还是 Team，任何成员的自述都不入状态，环境验证说了算；fail-closed 对每个成员独立生效；Observer 事件流新增 `subagent_start/end`、`task_claim/task_complete`。

**审计门的边界与强化（Zagens / LHH 论文）**：完成度天花板 = 模型自分解清单的完整性——模型漏画的交付物，审计也发现不了（LHH：错误的验收合同可能得到"认真核验过的错误答案"，如隐藏性能阈值、时间性证据、多义目标）。强化手段：**机器可回放信号**（退出码 / 路径命中 / 事件日志）优先于 LLM 判断（Machine Oracle）；**"换一个 LLM 盖章"不构成独立性**（审计者-建设者共谋确认）；有界轮次耗尽后诚实停机（`audit_unmet`）而非造假绿灯。MoS 审计门保持"环境只读验证 + 机器信号"，LLM 评审只作质量补充、不作完成判定；Replanning 是受控变更而非无限重试。

**自我进化（SICA / AlphaEvolve / DPO）**：三层梯度，从零风险到高风险——
- **知识进化**：执行历史沉淀为"经验记忆"（成功策略 / 已知陷阱入库，决策时检索注入），按成功率/时间/成本加权评分择优——复用 M2.7 记忆基础设施，零代码风险。
- **配置进化**：进化的对象是工具描述 / 系统提示词 / 路由与 Policy 规则——用 Golden 测试集做 A/B、LLM-as-a-Judge 结构化评分、版本化存储择优晋级（依赖 M4 评估）。
- **代码自修改**：独立"进化 Agent"（不参与主循环、只消费审计事件流）在工作区修改外围策略/工具代码 → 编译 → 跑测试 → 通过才归档；核心引擎人类控制，合并走人工批准（HITL 升级策略）。
核心约束：**没有外部基准就没有进化**——闭环必须有机器可回放信号（退出码 / 测试通过 / 轨迹匹配），禁止 LLM 自评进化（共谋确认）；进化梯度可控、每层可回滚（版本档案）；用 DPO 偏好数据直接更新而非 PPO 两步法（跳过奖励模型，更简单稳健）；推理扩展定律——小模型 + 多候选生成 + Elo 择优可超越单纯加大模型。

**插件化：Agent Plugins 1.0.0**（Google / AWS / Microsoft / OpenAI / Cursor / Vercel 联合的厂商中立目录规范，agent-plugins.org）：统一打包 Agent Skills 与 MCP 服务器的可移植目录格式——`plugin.json`（最小仅 `$schema` + `name`；schema 封闭，客户端特定数据走 `extensions`）+ `skills/`（SKILL.md：frontmatter 的 name + description 触发条件、步骤化正文、`references/` 按需加载）+ `mcp.json`（transport：stdio / streamable-http）+ 客户端命名空间目录（如 `mos.<org>/`，其他客户端忽略）。关键语义：**文件结构即契约**；组件独立校验（单个 skill 无效跳过继续、MCP 连接失败不阻塞其余组件加载）；规范不覆盖安装 / 权限 / 沙箱 / 来源验证——恰是 MoS 的 `agent.Policy`（M1.5）+ `agent.Sandbox`（M1.6）+ 人工网关（M2.8）的职责；客户端可增量实现（先支持 skills，MCP 可选）。参考 OpenAI/plugins 仓库：marketplace.json 提供插件目录索引 + policy{installation, authentication}。MoS 对齐：M2.6 Skills 懒加载直接采用 SKILL.md 格式；M3 MCP 接入读 mcp.json；M4 插件化采用此目录规范 + marketplace。

## 工程参考：工具规模化的业界方案（源码级对照）

MoS 当前工具注入为**全量原生 function calling**（`Registry.List()` 把全部工具 schema 注入每次请求），工具量增大后 token 随工具数线性增长。业界主流方案（本地源码逐一核实）：

| 项目 | 语言 | 工具注入策略 | 工具结果 / 历史瘦身 |
| --- | --- | --- | --- |
| Claude Code | TS | 原生 FC + deferred 懒加载：MCP 与 shouldDefer 工具只发名字（defer_loading），按消息历史"已发现"的 tool_reference 动态过滤；deferred 描述占上下文 10% 才启用检索（auto 阈值）；subagent / coordinator 角色白名单 | 单工具 50K 字符 / 单消息 200K 上限，超限落盘只给预览；isConcurrencySafe 分组最多 10 路并行；built-in 前缀排序 + 会话级 schema 缓存保 prompt cache 稳定 |
| DeerFlow | Python | MCP 工具默认 deferred：`<available-deferred-tools>` 只列名字，`tool_search` 取 schema 后 promote；PR2 关键字路由每次最多自动带 top_k(3) 个 schema；tool_groups + skill 白名单 + 子 Agent allow/deny 三级过滤 | ToolOutputBudgetMiddleware：超限输出落盘 + head/tail 预览 + read_file 按需回读；系统提示静态化复用前缀缓存 |
| OpenClaw | TS | **目录化最激进**：全量工具收编为 tool_search / tool_describe / tool_call 三个轻量控制工具（code-mode 仅 exec/wait）；directory 模式按 prompt 意图打分预选 ~4 个水合；隐藏工具调用时才惰性实例化；可用性信号（auth/config/env/插件）动态过滤 | 单条结果 ≤ 上下文 30%（16k/32k/64k 档）、聚合 ≤ 50%，head+tail 智能截断；prompt 前 + 会话历史双重截断，超压联动压缩；技能索引 18K 字符预算三档降级 |
| Pi | TS | 默认最小激活集 read/bash/edit/write 4 工具；系统提示每工具一行 promptSnippet（强制单行）；provider 级 splitDeferredTools（Anthropic tool_reference / OpenAI tool_search / Kimi system+tools）；扩展工具运行中经 addedToolNames 生长 | 输出限 2000 行 / 50KB，bash 截尾 / read 截头 / grep 单行 500 字符，给 fullOutputPath / offset 逃生通道；截断策略写进工具 description |
| LangChain | Python | 默认全量 bind_tools 不裁剪；源头压缩：过滤 InjectedToolArg / 隐藏参数、删 title/$defs；schema 生成 memo 缓存；v1 LLMToolSelectorMiddleware（小模型先选 N 个再注入）与 ProviderToolSearchMiddleware（defer_loading + 服务端 tool_search） | trim_intermediate_steps 保留最近 N 条；ClearToolUsesEdit 超 100K token 清旧工具结果以 [cleared] 占位保留 3 条；SummarizationMiddleware；asyncio.gather 并行 |
| OpenHarness | Python | 全量注入（对照样本）：每轮注册表全部 schema（含 MCP）；MCP schema 隐性压缩（只留类型丢描述）；状态外置 carry-over（任务焦点 / 已验证工作容量上限 + 截断，独立于消息历史） | 输出 >16K 落盘 + 3K 预览；microcompact 清旧工具结果保留 5 条 → context collapse 确定性截断 → LLM 全量摘要（tools=[] 强制纯文本）；asyncio.gather 并行 |

**六种策略族**：

1. **schema 源头压缩**（LangChain / OpenHarness）：删 title/$defs、过滤注入参数、MCP 只留类型——单工具减负，全量注入也能延缓爆炸
2. **deferred 懒加载**（Claude Code / DeerFlow / Pi / LangChain v1）：工具只发名字进 prompt，schema 经 tool_search / tool_reference 按需进入——Claude Code 的"按已发现集合过滤 + 10% 自动阈值"最成熟
3. **目录化控制工具**（OpenClaw）：全量收编为 2-3 个轻量控制工具，模型"搜索 → 查看 → 调用"三步触达——最激进，适合超大规模工具集
4. **LLM 工具选择器**（LangChain v1）：主模型前用小模型结构化输出选 N 个相关工具再注入
5. **工具集分组 / 裁剪**（DeerFlow / Claude Code / Pi / LangChain）：按 Agent 角色 / 技能白名单 / 激活集合过滤，每阶段只暴露相关工具（M3 多智能体天然具备）
6. **工具结果外置 + 历史瘦身**（全部项目）：输出超限落盘 / 截断 + 旧结果清理 + 静态系统提示保 prompt cache

**MoS 对齐**：M2.3 工具输出截断参考 Claude Code 落盘模式（阈值 + 预览 + read_file 回读）与 OpenClaw 预算占比（单条 30% / 聚合 50%）；M2.10 实现 schema 缓存与 deferred 懒加载；M3 子 Agent 工具集裁剪参考 DeerFlow tool_groups + Claude Code 角色白名单；M4 静态系统提示保 prompt cache 参考 DeerFlow（动态内容放 user 消息侧）。

## LLM 接入

通过协议注册表创建模型对象，协议适配器均实现统一的 `llm.ChatModel` 接口：

```go
reg := llm.NewRegistry()
reg.Register("openai", openai.New)      // OpenAI 兼容协议
reg.Register("anthropic", anthropic.New) // Anthropic 原生协议

model, err := reg.Create("openai", llm.Config{
	"api_key": os.Getenv("OPENAI_API_KEY"),
	"base_url": "https://api.deepseek.com/v1", // 可选，默认 OpenAI 官方地址
	"model":   "deepseek-chat",
})
```

配置键约定：`api_key` / `base_url` / `model`。

## 目录结构

双模块布局：**仓库根是纯依赖 SDK（`hegq.net/mos`，只含 `pkg/`）**，作为库集成到外部工程；**示例应用独立成工程（`examples/assistant`，`hegq.net/mos/examples/assistant`）**，通过 `replace hegq.net/mos => ../..` 本地引用 SDK。SDK 依赖方向单向 `pkg → 外部模块`，不含任何应用层入口。

```
mos/
├── go.mod                 # SDK 模块 hegq.net/mos（纯依赖库，仅 pkg/）
├── pkg/                   # SDK 框架层（对外公开 API）
│   ├── llm/               #   ChatModel 抽象（模型对象）+ 协议注册表
│   │   ├── openai/        #     OpenAI 兼容协议适配器
│   │   ├── anthropic/     #     Anthropic 原生协议适配器
│   │   └── gemini/        #     Google Gemini 协议适配器
│   ├── tool/              #   工具注册表、JSON Schema 生成、参数解析、执行沙箱
│   │   ├── sandbox.go     #     执行沙箱（M1.6）：jail + 超时 + 资源限制 + 环境裁剪
│   │   ├── builtin/       #     内置工具包：计算/时间/文件读写检索/网页抓取（纯标准库）
│   │   └── extension/     #     扩展工具包：shell（沙箱执行）/ web_search（搜索提供商）
│   │       └── search/    #       搜索提供商：duckduckgo / tavily / serper / brave / searxng
│   ├── memory/            #   会话记忆抽象
│   │   └── inmemory/      #     内存实现
│   ├── agent/             #   智能体运行时：任务循环 / Policy / Session / TaskState / Auditor / Observer
│   ├── planner/           #   规划策略：Planner 接口 + ReAct 实现
│   └── assistant/         #   组装好的完整智能体：模型对象注入 / 默认策略 / 内置+扩展工具包
└── examples/
    └── assistant/         # 示例应用（独立 Go module：hegq.net/mos/examples/assistant）
        ├── go.mod         #   replace hegq.net/mos => ../..（本地引用 SDK）
        ├── main.go        #   CLI 薄入口
        ├── internal/
        │   ├── config/    #     应用层：.env + 环境变量 → 模型对象 + 运行配置
        │   └── cli/       #     应用层 CLI 交互逻辑（REPL / TUI）
        ├── .env.example   #   应用层配置模板（复制为 .env 使用，MOS_CONFIG 可指定路径）
        └── .gitignore
```

### pkg 与示例应用 internal 的划分准则

**`pkg/` 放框架层面的能力，`examples/assistant/internal/` 放示例应用层面的能力。**

`internal/` 是 **Go 语言级约束**：其下包只能被父目录以内的代码 import，外部工程引用会直接编译报错（`use of internal package not allowed`）。判断归属时先问：

1. **这是框架层面的能力，还是应用层面的能力？** 框架层（通用、可复用、有 API 契约）→ `pkg/`；应用层（绑定具体产品的入口、配置约定、交互形态）→ 示例工程的 `internal/`
2. **外部工程集成 MoS 时需要直接 import 它吗？** 是 → `pkg/`；仅示例应用自己用 → 示例工程的 `internal/`
3. **它是稳定、带契约的公开 API，还是可随意重构的实现细节？** 前者 → `pkg/`；后者 → 示例工程的 `internal/`

当前划分示例：

| 归属 | 包 | 理由 |
| --- | --- | --- |
| `pkg/` | `llm` / `tool` / `memory` / `agent` / `planner` | 框架层通用基础能力 |
| `pkg/` | `tool/builtin` | 框架层内置工具包：纯标准库通用工具，随框架分发，`builtin.Default()` 一键装配 |
| `pkg/` | `tool/extension` | 框架层扩展工具包：shell（沙箱执行）/ web_search（搜索提供商，实现见 `extension/search`），需依赖注入 |
| `pkg/` | `assistant` | 框架层组装智能体（模型对象注入，`Config` 仅含运行配置） |
| `pkg/` | `llm/openai` `llm/anthropic` `llm/gemini` | 框架层协议适配器，外部按需选用 |
| `examples/assistant/internal/` | `config` | 示例应用层：从 `.env` + `MOS_*` 环境变量构建模型对象与运行配置 |
| `examples/assistant/internal/` | `cli` | 示例应用层：REPL/TUI 绑定终端交互，装配内置 + 扩展工具包 |

注意：框架层 `assistant.Config` 只含系统引导、迭代次数与框架概念对象（`Policy` / `Planner` / `Auditor` / `Memory` / `Observer`，nil 时用框架默认实现）；**模型、工具与概念对象均由外部注入**（`llm.ChatModel` + `[]tool.Tool` + 概念实例）。"如何从环境变量/配置文件构建模型对象、装配哪些工具、构造安全策略"属于应用层约定（放示例工程 `examples/assistant/internal/config` / `internal/cli`）。外部工程可按自己的配置体系注入模型、工具与概念实现。

## 测试

双模块分别测试：

```bash
# SDK（仓库根）
go test ./...

# 示例应用
cd examples/assistant && go test ./...
```

适配器测试通过 `httptest` 模拟协议服务，验证请求构造与响应解析的正确性，不依赖真实网络。

## 路线图

方向参考 LongHorizon-Harness 的"审计式状态迁移"（状态外置、只接受被环境独立验证过的事实、执行上下文可刷新）、Pi-Agent 的"上下文工程"（工具输出截断、结构化压缩、两层消息翻译边界）、Agentic Design Patterns 的"路由 / 异常处理 / 人机协同"、Harness Engineering 的铁律（上下文越少越好、专才 > 通才、状态写文件不写上下文、能写成 Linter 的约束别写成文档）与九种编排模式谱系（ReAct → Plan&Solve → REWOO → Storm → LLMCompiler → Basic Reflection → Reflexion → LATS → Self-Discover）、Loop Engineering 的九杠杆（确定性传感器 / 显式停止条件 / 跨迭代上下文管理 / 清晰工具设计 / Doom Loop 检测 / 上下文利用率 40% Dumb Zone 阈值 / 补偿面理论——harness 随模型变强主动拆组件，Anthropic 已拆 Context Reset）、Graph Engineering 的图编排四原语（节点 / 边 / 状态 schema + reducers / checkpointer 中断恢复，LangGraph 参考）与使用判据（分支 / 并行 / 持久 / 可审计——不满足判据不引入显式图）、Agent Plugins 1.0.0 的插件打包规范（plugin.json + skills/ + mcp.json + 客户端命名空间，组件独立校验）。

- [x] **M1**：Agent 任务循环、多协议 LLM 接入、工具系统、工作记忆、事件观测
- [x] **M1.5**：组装智能体（assistant 包 + CLI）、Harness Engineering 工具安全防护、Loop Engineering 循环可观测
- [x] **M1.6**：执行沙箱（隔离层）——`tool.Sandbox`：Policy 管"该不该调用"、沙箱管"放行后能造成多大破坏"；本地轻量实现 = 工作目录 jail + 单次调用超时（进程组终止）+ 输出上限（丢弃式截断）+ 进程资源限制（CPU / 虚拟内存 / 单文件，`ulimit` 预置）+ 环境裁剪（白名单）+ 网络控制（NoNet 剥离代理，尽力而为），纯 Go 标准库零依赖；ctx 注入 `WithSandbox` / `SandboxFrom`，agent runtime 统一注入，shell 工具优先沙箱执行；远程强隔离（M4）可换 Docker / containerd 后端，对外 API 不变
- [x] **M2.1**：任务状态外置（`TaskState`：目标 / 已验证事实 / 状态机，独立于消息历史，跨轮可恢复）
- [x] **M2.2**：审计门（`Auditor`：默认审计器仅接受白名单只读工具的可验证输出，模型自述不算数）
- [ ] **M2.3**：工具输出截断（行数/字节双重限制 + read 截头 / bash 截尾 + 逃生通道提示，防单次工具结果撑爆窗口）
- [ ] **M2.4**：两层消息与翻译边界（内部结构化消息 → LLM 标准消息 `convertToLlm`，自定义消息转 user 角色，支持摘要注入与 `excludeFromContext`）
- [ ] **M2.5**：fresh-context 压缩（两轮之间触发，切割点不切断 ToolCall-Result 对；压缩摘要直接采用审计写入的 `TaskState`，而非 LLM 重新总结）+ 上下文利用率监控（40% Dumb Zone 告警线：渐进式披露——先给骨架、按需展开；分层上下文——常驻摘要 / 按需展开 / 懒加载）+ Doom Loop 检测（相同工具连续 3+ 次相似调用 → 回退上一有效状态或重置上下文）+ 上下文重置 / handoff（压缩后过渡到新一轮，跨 Session 交接，对齐 Loop Engineering 九杠杆）
- [ ] **M2.6**：系统提示词动态组装（按"系统约束 / 任务定义 / 当前状态 / 工具签名 / 历史摘要"分段槽位；克制主义——静态模板应足够小（Pi 仅约 1K token 对比 Claude Code 数万），固定指令占得越少留给任务上下文越多；多级 CLAUDE.md 向上递归 + Skills 懒加载清单（对齐 Agent Skills 规范：SKILL.md frontmatter 声明 name + description 触发条件 + 步骤化正文 + references/ 按需 read））
- [ ] **M2.7**：长期记忆（sqlite-vec 向量检索 + SQLite FTS5 的 BM25 混合检索）、RAG（分块策略 chunk_size/overlap + 检索注入 + 来源引用）、Agentic RAG 知识守门人（源验证 / 冲突裁决 / 多步检索 / 知识差距触发外部工具）、流式输出透传
- [ ] **M2.8**：人工介入工具 + 升级策略（`request_human_input`：模型主动请求人类输入/确认/授权，作为普通工具接入；升级策略定义"何时、如何交给人类"，配合人工网关；注意 HITL 权衡：可扩展性不足、依赖专家、敏感数据需匿名化）+ interrupt-before（等待人工输入前用 Checkpointer 持久化状态，恢复后从断点继续，对齐 Graph Engineering 的 checkpointer 语义）
- [ ] **M2.9**：经验记忆（知识进化第 1 层：执行历史沉淀为成功策略 / 已知陷阱库，按成功率 / 时间 / 成本加权评分择优，决策时检索注入——复用 M2.7 记忆基础设施，零代码风险）
- [ ] **M2.10**：工具规模化——schema 生成缓存（`generateSchema` 反射结果按工具 memo 化，避免循环内每轮重复）+ schema 压缩（删 title / $defs、过滤注入参数）+ deferred 懒加载（`tool_search` 工具按需取 schema，名字清单进 prompt；工具描述占上下文超阈值自动启用，小工具集保持全量注入，参考 Claude Code 10% 自动阈值）+ 静态系统提示保 prompt cache（动态内容放 user 消息侧，参考 DeerFlow）
- [ ] **M3**：多智能体编排——Coordinator 路由（按意图分派：LLM 路由处理模糊输入、规则路由保证确定性可审计）+ 编排拓扑可选（监督者 / 层次化 / 批评者-审查者，先落地监督者再演进）+ 三权分离 Manager / Executor / Auditor（审计式）+ SubAgent 子智能体（声明式定义：`name` / `description` 自动委派匹配 / `model` 覆盖 / `tools` 白名单即 blast radius 收缩 / 系统提示词正文，对齐 Claude Code `.claude/agents/` frontmatter；首选 **agents-as-tools** 形态——SubAgent 实现 `tool.Tool` 包装进 Registry，主 Agent 经 function calling 调用只收结构化结果；handoff 形态走 Coordinator 路由；独立 Context、中间工具调用留内部、回传自述仍须 `Auditor` 验证；决策判据——instructions/tools/policy 真正改变才拆）+ 双阶段架构（Initializer 写 plan.md → Executor 接力，不共享 Context Window，即 Plan&Solve Replanner）+ Basic Reflection（Generator / Reflector / Revisor 自查循环，Reflector 用独立 Context 避免"自我合理化"污染；Reflexion 变体引入外部数据评估）+ LLMCompiler 并行 DAG（可选模式，Map-Reduce：分支并发 + 共享状态汇聚 + 合并只读分支摘要）+ Elo 择优（多方案比较排序）+ 合约化任务（可验证交付物 + 协商澄清 + 子合约分解）+ MCP 接入（五步交互：发现 → 请求制定 → 调用 → 错误回传 → 结果注入；mcp.json 声明服务器与 transport：stdio / streamable-http）；Agent Loop 升级（Turn 语义 + 批量工具执行 + 错误注入反馈——错误信息解释"为何违反规则 + 正确做法"）+ Graph Engineering 显式图边界（节点 / 边 / 状态 schema + reducers / checkpointer；编译期校验——节点可达性、状态字段一致性、边类型合法性，不合法图拒绝运行；仅在满足分支 / 并行 / 持久 / 可审计判据时引入，LangGraph 参考）+ AgentTeam 智能体团队（Team Lead 拆解 / 创建队友 / 协调 / 汇总 + Teammates 独立 Agent 实例 + Shared Task List 共享任务队列——`pending → in_progress → completed` + `blocks/blockedBy` 依赖 + 自动解锁，字段级契约对齐 TaskState + Mailbox 文件即消息的点对点通信 + 状态落盘（角色 / 工作上下文 / 决策 / 任务文件 / 团队名册写 `runs/<run-id>`，中断后一条命令重建，对齐 ATWZ）+ 工作树隔离（队友独立工作目录 / 分支，测试通过才合并，防文件冲突））
- [ ] **M4**：生产化——异常处理与恢复（错误检测：工具输出验证 / API 错误码 / 超时；错误处理：日志 / 重试 / 回退 / 优雅降级 / 通知；恢复：状态回滚 / 诊断 / 自我纠正 / 升级）+ 沙箱强隔离后端（L3：Docker / containerd，替换 M1.6 轻量实现，`agent.Sandbox` 接口不变）+ 模型分级路由（按任务复杂度选档：简单→经济模型、复杂→强模型，附顺序回退链）+ 评估与监控（轨迹评估 Golden 测试集 + LLM-as-a-Judge 结构化评分 + 漂移检测 + 配置 A/B 择优：工具描述 / 提示词 / 路由与 Policy 规则版本化晋级，即知识进化第 2 层）+ 优先级调度（紧急性 / 重要性 / 依赖关系，限流配额按优先级区分）+ 生命周期控制 API（启动 / 暂停 / 恢复 / 终止）、`runs/<run-id>` 可恢复运行目录（Workspace 即真相：状态 / 中间结果 / 审计回放走文件系统，断点续传）、人工网关、Dashboard 可视化、限流 / 重试 / 缓存、熵管理（后台文档园丁定时清理过期文档 / 架构漂移）、插件化（对齐 Agent Plugins 1.0 目录规范：plugin.json + skills/ + mcp.json + 客户端命名空间目录；组件独立校验——单个 skill 无效跳过、MCP 失败不阻塞其余加载；marketplace 索引 + 安装/认证策略；MoS 特定数据走 extensions / 命名空间，不污染可移植字段）
- [ ] **M5**：自我进化第 3 层（实验性）——独立"进化 Agent"不参与主循环、只消费审计事件流，在工作区修改外围策略/工具代码 → 编译 → 跑测试 → 通过才归档；核心引擎人类控制，合并走人工批准；版本档案支持回滚（SICA / AlphaEvolve / DPO 参考）
