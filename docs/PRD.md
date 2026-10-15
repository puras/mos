# MoS PRD（产品需求文档）

> 版本：v0.1（草案）｜ 日期：2026-10-15 ｜ 维护：puras
> 需求权威来源：`docs/architecture-v2.md`（架构与路线图）+ `README.md` 路线图 + `docs/sdk-contracts.md`（平台契约）；本文将其整理为可跟踪的产品需求视图。
> 状态约定：M1 ~ M2.8+ 为**已交付基线**（本文 §4 只盘点不展开）；M2.9 ~ M5 为**待建需求**（本文 §5/§6 的需求主体）。API 细节以 `docs/agent/*` 子系统文档与源码为准。

---

## 1. 产品概述

### 1.1 一句话定位

MoS（取自 Sapient）是基于 Go 的**智能体基础框架（Agent Base SDK）**：提供统一 LLM 接入、声明式工具系统、事件日志会话、可插拔循环驱动与安全纵深防御，帮助快速构建智能体应用与智能体平台。

### 1.2 解决的问题

在模型能力之上，长程任务的可靠性瓶颈在 **harness**（驾驭层）：上下文腐化、工具误用、不可验证的"自我声明式完成"、无法断点恢复、缺乏审计。MoS 把这些工程问题沉淀为可复用的框架能力：

- **状态写文件不写上下文**：`TaskState` 外置、会话事件日志（append-only）是唯一事实来源；
- **只接受环境独立验证过的事实**：`Auditor` 审计门，模型自述不算数；
- **上下文越少越好**：截断（M2.3）+ fresh-context 压缩（M2.5）+ 槽位式系统提示词 + Skills 懒加载（M2.6）；
- **能写成 Linter 的约束别写成文档**：`Policy` 执行前拦截 + `GuardBus` 单调守卫 + `Sandbox` 执行中隔离。

### 1.3 三层职责边界（MoS 是 SDK，不越界）

| 层 | 归属 | 职责 |
| --- | --- | --- |
| MoS | 本仓库（SDK 内核） | Agent 任务循环、工具系统、会话/审计/状态、上下文工程、钩子守卫、审批与断点恢复、事件观测 |
| SaaS 平台 | 独立平台（MOSSCloud） | HTTP/WS API、多租户、鉴权、任务调度、计费、Dashboard、对象存储、人工确认通道 |
| 协议层 | `github.com/puras/moproto`（本地 replace） | LLM 协议归一、密钥管理、模型路由（发布前待办：拆独立 module，见 FR-A1） |

MoS 明确不做：HTTP 服务、多租户、任务队列、计费、可视化；不操作数据库（持久化只提供 `Persistence` 接口 + 内存/JSONL 默认实现，数据库后端由业务层注入）；不做运行期插件热加载（只做装配时选组件）。

### 1.4 设计理念（需求推导原则）

Harness Engineering 四铁律 + Loop Engineering 九杠杆 + Audit Gate 审计式状态迁移 + Graph Engineering 判据门控（满足分支/并行/持久/可审计才引入显式图）+ 补偿面理论（harness 随模型变强主动拆组件）。详见 README「工程参考」章节。

---

## 2. 目标用户与使用场景

| 用户 | 场景 | 入口 |
| --- | --- | --- |
| 应用开发者 | 在 Go 工程中嵌入一个开箱即用的智能体（工具 + 安全策略 + 审计） | `assistant.New` 一行装配 |
| 平台开发者 | 自建智能体平台：声明式配置装配、挂起-恢复、事件流对接观测 | `agent/config`（YAML → Agent）、`agent/handle`、`session.Persistence` |
| 框架研究者 | 理解/扩展 agent 运行时（驱动、钩子、守卫、seam） | `examples/agentcli`（七站式教程 CLI）、`docs/agent/*` |
| 业务集成方 | 换模型/换后端而不改业务代码 | `llm.Registry` + `agent/seam` 按名替换组件 |

---

## 3. 产品目标与非目标

### 3.1 目标（G）

| 编号 | 目标 | 度量 |
| --- | --- | --- |
| G1 | 循环可替换：ReAct / Plan&Execute / Reflection / Scripted 按 `LoopDriver` 实现，任务可路由切换 | 新增一种循环 = 一个 `LoopDriver.Run` 实现，不污染共享设施 |
| G2 | 环节可改写：模型输入 / 工具执行前后等环节可被钩子改写，安全约束不可被钩子绕过（HookBus / GuardBus 双轨制） | 纵深防御测试覆盖（守卫绕过不可行） |
| G3 | 提供者可替换：Sandbox / Memory / Retriever / Policy / Driver 经 `agent/seam` 按名注册、YAML 装配 | 纯配置可组装可运行 Agent（`config.Assemble`） |
| G4 | 会话可重放可恢复：append-only 事件日志 + `Restore` + Checkpoint 挂起-续跑 | 崩溃恢复测试（孤儿 turn 合成 interrupted）；审批挂起-恢复 E2E |
| G5 | 安全纵深：Policy（规则层）→ Sandbox（隔离层）→ 审批（人工层）→ 审计（验证层）四层防线 | fail-closed 纪律全绿（NFR-1） |
| G6 | 近零依赖：SDK 仅依赖协议层库 + `google/uuid` + `gopkg.in/yaml.v3` | `go.mod` require 审计 |

### 3.2 非目标（不做）

产品层（Web UI / 网关 / 调度 / 多租户 / 计费）、数据库驱动与迁移、运行期 DI 容器、模型训练与微调、CLI 产品化（`examples/*` 仅为示例与学习材料）。

---

## 4. 已交付基线（Current，M1 ~ M2.8+）

### 4.1 分层架构（已全部落地）

```text
装配层：assistant.New（模型对象 + 工具集 + 运行配置注入，nil 用框架默认）
驱动层：LoopDriver × 4（ReAct / PlanExecute / Reflection / Scripted）+ DriverRouter 任务路由
执行通道：agent/execpath（Policy 守卫 → 批量并发执行 → 审计回填，三驱动共用）
基础设施：EventBus / HookBus / GuardBus / Session 事件日志 / RunContext / Scope / Telemetry seam
现有基础：llm（+transport 共用 envelope）/ tool（builtin + extension）/ planner / memory
Capability：filesystem / sandbox / httpx 顶层包（v2.24 从 tool 包剥离）
```

### 4.2 能力盘点（按域）

| 域 | 已交付能力 |
| --- | --- |
| LLM 接入 | `ChatModel` 统一抽象；OpenAI 兼容 / Anthropic / Gemini 适配器（函数调用 + 流式 + FinishReason 透传 + ReasoningEffort→extended thinking）；`llm/transport` 共享 HTTP envelope 与用量归一化；协议注册表 + 5 个标准装饰器（Logging/Retry/Timeout/Fallback/Cache） |
| 工具系统 | 结构体 tag → JSON Schema、参数容错解析；`tool/builtin` 7 个纯标准库工具；`tool/extension` web_search（5 家搜索提供商）；输出截断（行/字节 + 智能截断 + 落盘逃生通道）；`Sandbox` 进程级隔离（jail + 超时 + 资源限制 + 环境裁剪 + 网络控制）；`filesystem` capability（Lexical + Realpath 双层校验、symlink TOCTOU 防护、`fs/resolve` 审计事件） |
| 安全 | `DefaultPolicy` 分级（绝对危险拒绝 / 受限危险 WorkDir + Approval 确认 / 前缀白名单 / 路径越界 / fail-closed 未知工具）；`request_human_input` 人工介入工具；异步 Approval 挂起-恢复 |
| 会话与状态 | append-only 事件日志 + `deriveMessages` 投影；JSONL 持久化（fail-fast、崩溃恢复）；`TaskState` 外置（目标/已验证事实/状态机）；`Auditor` 审计门（只读白名单 + Trust 扩展） |
| 上下文工程 | 截断 + fresh-context 压缩（不切断 ToolCall-Result 对，摘要 = TaskState 快照）；槽位式系统提示词 + CLAUDE.md 向上递归；Skills（7 字段 frontmatter + `activate_skill` + `allowed-tools` 守卫硬收窄，激活作用域 = 本次 Run）；40% Dumb Zone 告警；Doom Loop 守卫 |
| 循环生态 | 4 驱动 + HybridRouter（规则硬路由 → LLM 分类兜底）；批量工具三阶段并发执行（preflight → 有界并发 → 按源序回填）；`tool.SequentialTool` 串行回退 |
| 生命周期 | `agent/scope` 注册作用域（逆序回滚）；`agent/approval` + `agent/checkpoint` + `agent/handle`（create/resume/dispose + 资源校验）；TaskState 持久化 |
| 可观测 | `Observer` 回调 + `EventBus`（emit/waterfall/parallel/serial 四模式）；Telemetry seam（显式 span 契约 + Recorder 参考实现 + Conformance 契约测试）；`skill/*`、`fs/resolve`、`approval/*`、`checkpoint/*` 等事件全量落会话日志 |
| 装配 | `assistant.Config` 全量注入式配置；`agent/seam` + `agent/config`（YAML → Agent）；声明式驱动选择（`Driver` / `DriverFactory`） |
| 示例与文档 | `examples/cli`（真实模型）、`examples/protocli`（双协议切换 + Wrap 组合）、`examples/agentcli`（七站式运行时教程）；`docs/agent/` 18 篇子系统文档 + `docs/assistant.md` + `docs/llm.md` |

### 4.3 里程碑交付记录

| 里程碑 | 内容 | 状态 |
| --- | --- | --- |
| M1 / M1.5 / M1.6 | 任务循环、多协议接入、工具系统、事件观测 / assistant 组装 + Policy / 执行沙箱 | ✅ |
| M2.1 ~ M2.7 | 状态外置、审计门、输出截断、两层消息翻译边界、fresh-context 压缩 + 利用率监控 + Doom Loop、槽位系统提示词 + Skills、RAG 扩展点 | ✅ |
| v2 架构重构（v2.3 ~ v2.17） | EventBus / Session 日志 / Hook+Guard / RunContext+LoopDriver / seam / router / executor / scope / 持久化 / telemetry | ✅ |
| M2.8 / M2.8+ | 异步 Approval + Checkpointer + AgentHandle / 文件工具 Jail（破坏性变更：`WorkspaceRoot` 必填） | ✅ |
| M2.9 ~ M5 | 见 §5 需求 | ❌ 未开始 |

---

## 5. 功能需求（Roadmap → FR）

优先级定义：**P0** = 发布/下一里程碑阻塞项；**P1** = 本版本目标；**P2** = 规划中、实施前需修订细化。

### 5.1 FR-A：发布前待办（P0）

| 编号 | 需求 | 验收标准 |
| --- | --- | --- |
| FR-A1 | 协议层 `moproto` 脱离本地 `replace`：拆分为独立 module 或发布到可达仓库（对齐 AGENTS.md「对外发布前需拆分协议层」待办）；MoS 对协议层只消费 `pkg/` 纯定义包，**协议仓库只读**（缺陷与建议走 PR/issue，不直接改） | 外部工程 `go get github.com/puras/mos` 无需本地路径即可构建；`go.mod` 无本地 replace |
| FR-A2 | 文档与实现同步：README 等处 import 路径 `hegq.net/mos` → `github.com/puras/mos`；FEATURES.md 更新记录补齐 2026-09 ~ 10 变更（capability 拆分、`llm/transport`、moproto 迁移、agentcli/docs 新增） | 文档 grep 无旧模块路径；新贡献者按 README 可跑通 |

### 5.2 FR-B：M2.9 经验记忆（知识进化第 1 层，P1）

| 编号 | 需求 | 验收标准 |
| --- | --- | --- |
| FR-B1 | 执行历史沉淀为经验库：成功策略 / 已知陷阱入库，按成功率 / 时间 / 成本加权评分择优 | 经验写入 → 评分 → 检索 → 注入决策链路 E2E 测试 |
| FR-B2 | 决策时检索注入：复用 M2.7 `memory.Retriever` 基础设施与注入钩子 | 未注入经验时行为与现状完全一致（零回归）；注入走 `CustomMsg(Source="experience")`，不进会话历史 |

### 5.3 FR-C：M2.10 工具规模化（P1）

| 编号 | 需求 | 验收标准 |
| --- | --- | --- |
| FR-C1 | schema 生成缓存：`generateSchema` 反射结果按工具 memo 化 | 循环内不再重复反射（基准测试佐证） |
| FR-C2 | schema 压缩：删 title / $defs、过滤注入参数 | 注入 schema 体积下降且参数解析不受影响 |
| FR-C3 | deferred 懒加载：工具只发名字进 prompt，`tool_search` 按需取 schema；工具描述占上下文超阈值（参考 Claude Code 10%）自动启用，小工具集保持全量注入 | 工具数增长时 prompt token 亚线性；默认行为对现有用户不变 |
| FR-C4 | 静态系统提示保 prompt cache：动态内容移至 user 消息侧 | 连续两轮请求 system 段逐字节一致（缓存命中） |

### 5.4 FR-D：M3 多智能体编排（P1，建议分期 M3a / M3b）

| 编号 | 需求 | 关键约束 / 验收标准 |
| --- | --- | --- |
| FR-D1 | Coordinator 路由与编排拓扑：按意图分派（规则硬路由保证确定性可审计，LLM 分类兜底）；拓扑先「监督者/层次化」，后演进「批评者-审查者」 | 基于 `agent/router` 扩展；路由决策落会话日志可回放 |
| FR-D2 | 三权分离 Manager / Executor / Auditor：派发、执行、审计职责分属不同角色 | 审计不变式对每个成员独立生效（NFR-5） |
| FR-D3 | SubAgent 子智能体（首选 agents-as-tools 形态）：声明式定义（`name` / `description` 自动委派匹配 / `model` 覆盖 / `tools` 白名单 / 提示词正文，对齐 `.claude/agents/` 风格）；实现 `tool.Tool` 包装进 Registry，主 Agent 经 function calling 调用只收结构化结果 | 工具白名单即 blast radius 收缩（fail-closed）；独立 Context，中间工具调用留内部；回传自述仍须 Auditor 验证；决策判据入文档——instructions/tools/policy 真正改变才拆 |
| FR-D4 | 双阶段架构（Plan&Solve）：Initializer 写 plan.md 退出 → Executor 接力执行，不共享 Context Window；Replanner 随新约束修订计划 | 计划文件落 `runs/<run-id>`（依赖 FR-E6 可先落工作目录）；跨窗口接力 E2E |
| FR-D5 | LLMCompiler 多智能体并行 DAG：基于 `agent/driver/scripted`（单智能体先行版已落地）扩展 Map-Reduce——分支并发、结果经 TaskState 汇聚、合并只读分支摘要、并行度作为计划元数据 | DAG 校验 fail-closed（依赖环 / 节点存在性）；并发分支数有界 |
| FR-D6 | AgentTeam 智能体团队：Team Lead + Teammates + Shared Task List（`pending → in_progress → completed` + `blocks/blockedBy` 依赖自动解锁）+ Mailbox 点对点消息；状态落盘（角色/上下文/决策/任务文件/名册写 `runs/<run-id>`，中断后可重建）；工作树隔离（独立工作目录/分支，测试通过才合并） | 事件 `subagent/*`、`task_claim/task_complete` 落日志；团队重建演练（kill 后一条命令恢复） |
| FR-D7 | 合约化任务 + Elo 择优：可验证交付物 + 协商澄清 + 子合约分解；多方案生成按 Elo 排序择优 | 交付物可机器验证（退出码 / 路径命中 / 事件日志），禁止 LLM 自评闭环 |
| FR-D8 | MCP 接入：`mcp.json` 声明服务器与 transport（stdio / streamable-http）；五步交互（发现 → 请求制定 → 调用 → 错误回传 → 结果注入） | MCP 工具进 Registry 后与内置工具同受 Policy/Guard 管辖；单个 MCP 失败不阻塞其余加载 |
| FR-D9 | Agent Loop 升级：Turn 语义完善 + 错误注入反馈（错误信息解释"为何违反规则 + 正确做法"，Linter 报错即上下文工程） | 错误注入后模型自纠成功率可观测（测试集） |
| FR-D10 | 显式图边界（判据门控）：节点 / 边 / 状态 schema + reducers / checkpointer；编译期校验（节点可达性、状态字段一致性、边类型合法性），不合法图拒绝运行 | **仅在满足分支/并行/持久/可审计判据时引入**——图是可选编排层，不是默认架构 |

### 5.5 FR-E：M4 生产化（P1）

| 编号 | 需求 | 验收标准 |
| --- | --- | --- |
| FR-E1 | 异常处理与恢复三阶段：检测（工具输出验证 / API 错误码 / 超时）→ 处理（日志/重试/回退/优雅降级/通知）→ 恢复（状态回滚 / 诊断 / 自我纠正 / 升级） | 注入故障演练集通过；升级路径接 Approval |
| FR-E2 | 沙箱强隔离后端（L3）：Docker / containerd 替换进程级实现 | `sandbox.Runner` 接口不变，后端可切换；越狱测试通过 |
| FR-E3 | 模型分级路由：按任务复杂度选档（简单→经济模型、复杂→强模型），附顺序回退链 | 分级决策可配置可审计；回退链演练通过 |
| FR-E4 | 评估与监控（知识进化第 2 层 = 配置进化）：轨迹评估 Golden 测试集 + LLM-as-a-Judge 结构化评分 + 漂移检测 + 配置 A/B 择优（工具描述 / 提示词 / 路由与 Policy 规则版本化晋级） | 评估跑批可重复；版本化档案支持回滚 |
| FR-E5 | 优先级调度与配额：紧急性/重要性/依赖关系，限流配额按优先级区分 | 调度策略接口化（策略由平台注入） |
| FR-E6 | 生命周期控制 API + `runs/<run-id>` 可恢复运行目录：启动/暂停/恢复/终止；状态/中间结果/审计回放走文件系统（Workspace 即真相），断点续传 | kill -9 后可从运行目录恢复续跑 |
| FR-E7 | 人工网关 + Dashboard 可视化（平台侧消费 SDK 事件流实现） | 事件契约（`docs/sdk-contracts.md` §5）稳定不再变更 |
| FR-E8 | 限流 / 重试 / 缓存（横切能力下沉至 llm 装饰器与 transport 层） | 指标可观测 |
| FR-E9 | 插件化：对齐 Agent Plugins 1.0 目录规范（`plugin.json` + `skills/` + `mcp.json` + 客户端命名空间目录）；组件独立校验（单个 skill 无效跳过、MCP 失败不阻塞其余）；安装/权限/来源验证走 MoS 的 Policy/Sandbox/Approval | 合规 plugin 目录可被 `assistant.New` 加载；可移植字段不污染 |
| FR-E10 | M2.8 收尾：Approval TTL 自动超时、checkpoint 加密/签名（多句柄并发恢复维持业务层职责） | TTL 到期 fail-closed（默认拒绝）；checkpoint 防篡改校验 |

### 5.6 FR-F：M5 自我进化第 3 层（P2，实验性）

| 编号 | 需求 | 关键约束 |
| --- | --- | --- |
| FR-F1 | 独立"进化 Agent"：不参与主循环、只消费审计事件流，在工作区修改外围策略/工具代码 → 编译 → 跑测试 → 通过才归档；核心引擎人类控制，合并走人工批准；版本档案支持回滚 | 硬约束：没有外部基准就没有进化——闭环必须有机器可回放信号，禁止 LLM 自评进化（共谋确认）；每层可回滚 |

---

## 6. 非功能需求（NFR）

| 编号 | 需求 | 现状 / 度量 |
| --- | --- | --- |
| NFR-1 | **fail-closed 纪律**：未知工具拒绝、`WorkspaceRoot` 必填、ctx 缺 capability 即 `NotConfiguredError`、截断工具调用 fail-closed、审批未配置即拒绝、评审器缺失驱动拒运行 | 已执行；新增能力一律按此纪律设计并有测试 |
| NFR-2 | **防御性 nil-safe**：`RunContext` 方法对 nil 接收者安全；扩展点 nil 时降级为框架默认或 no-op，行为不变 | 已执行 |
| NFR-3 | **零开销降级**：Telemetry / Observer / EventBus 未注入时零额外开销 | no-op 路径基准测试 |
| NFR-4 | **日志即事实**：会话日志 append-only、`Append` 同步落盘 fail-fast、事件 Seq 连续可重放；日志不允许截断（崩溃恢复只补事件不改写） | 已执行；重放测试覆盖 |
| NFR-5 | **审计不变式**：任何智能体（含 SubAgent / 队友）的自述不入 `TaskState`，环境验证说了算；机器可回放信号优先于 LLM 判断 | 已执行；M3 多智能体延续 |
| NFR-6 | **依赖纪律**：近零依赖（协议层 + uuid + yaml.v3）；协议仓库只读（AGENTS.md 硬规则）；MoS 只消费协议层 `pkg/` 纯定义包，不依赖网关实现层 | `go.mod` 审计 + import 边界检查 |
| NFR-7 | **可测试性**：适配器用 httptest 协议模拟不依赖真实网络；seam 提供契约测试（`telemetry.Conformance` 模式推广） | `go test ./...` 全绿（注：本机沙箱禁本地端口监听时，httptest 类测试需在常规环境跑） |
| NFR-8 | **API 演进纪律**：未发布、无兼容负担，可按目标架构直接落地；但破坏性变更（如 `WorkspaceRoot` 必填）必须在文档更新记录显式声明 | 更新记录惯例执行 |

---

## 7. 里程碑规划

| 里程碑 | 内容（对应 FR） | 优先级 | 依赖 |
| --- | --- | --- | --- |
| **R（发布准备）** | FR-A1 协议层独立发布、FR-A2 文档同步 | P0 | moproto 仓库就绪 |
| **M2.9** | FR-B1/B2 经验记忆 | P1 | 无（复用 M2.7 扩展点） |
| **M2.10** | FR-C1~C4 工具规模化 | P1 | 无 |
| **M3a** | FR-D1/D2/D3/D8/D9（路由拓扑、三权分离、SubAgent、MCP、Loop 升级） | P1 | R |
| **M3b** | FR-D4~D7/D10（双阶段、并行 DAG、AgentTeam、合约+Elo、显式图） | P1 | M3a |
| **M4** | FR-E1~E10 生产化 | P1 | M3a（部分项可并行） |
| **M5** | FR-F1 自我进化（实验） | P2 | M4 的 E4（评估与机器信号） |

> 排期原则：不设日历日期，按依赖顺序推进；每个里程碑完成后在 `docs/architecture-v2.md` 更新记录与 FEATURES.md 同步状态。

---

## 8. 风险与开放问题

### 8.1 风险

| # | 风险 | 缓解 |
| --- | --- | --- |
| R1 | 规则层黑名单可被解释器入口/混淆命令绕过（README「诚实的边界」），进程级沙箱非强隔离 | 安全边界定位为纵深防御之一；FR-E2 强隔离后端是最终答案；SaaS 部署强制 `saas-jailed` preset |
| R2 | 审计门天花板 = 模型自分解清单的完整性（漏画的交付物审计也发现不了）；"换一个 LLM 盖章"不构成独立性 | 保持环境只读验证 + 机器信号；LLM 评审只作质量补充不作完成判定；有界轮次耗尽诚实停机（`audit_unmet`） |
| R3 | M3 范围过大（10 项 FR），一次性实施易失控 | 分期 M3a/M3b；逐项按决策判据门控（不满足判据不引入：SubAgent「合同论」、显式图四判据） |
| R4 | 文档与实现漂移（README 旧模块路径、FEATURES 更新记录滞后于 capability 拆分 / transport / moproto 迁移） | FR-A2 收口；维持「每次修订顶部追加更新记录」惯例 |
| R5 | 上下文工程经验参数（40% Dumb Zone、Doom Loop 阈值 3、KeepRecent 12）随模型迭代可能失效 | 补偿面理论：随模型变强复测并主动简化 harness；参数全部可配置 |
| R6 | 协议层经本地 `replace` 开发，外部协作/CI 无法复现 | FR-A1 发布前收口；过渡期在 README 声明本地依赖前置步骤 |

### 8.2 开放问题（待用户/维护者决策）

1. `moproto` 拆独立 module 的时间点与版本策略（是否与 MoS v1.0 同步）？
2. MCP transport 优先级：stdio 与 streamable-http 先做哪个？
3. 远程沙箱后端选型：Docker 还是 containerd（或两者都做适配器）？
4. M2.9 经验记忆的默认介质：SDK 内置 JSONL 默认实现（对齐 Persistence 惯例）还是纯扩展点？
5. M3a 中 SubAgent 与 MCP 的先后（MCP 工具面会放大 SubAgent 工具白名单的价值，但也增加交付面）？

---

## 9. 术语与引用

- 术语表：`docs/architecture-v2.md` §12（概念与名词，按模块分组，标注现有/规划状态）。
- 子系统详设：`docs/agent/`（approval / capabilities / checkpoint / config / driver×4 / event / execpath / handle / hook / scope / session / system / telemetry）+ `docs/assistant.md` + `docs/llm.md`。
- 平台集成契约：`docs/sdk-contracts.md`（职责边界 / 事件契约 / 状态序列化契约）。
- 历史功能清单：`FEATURES.md`（含更新记录，注意与本文 §4 的分工：FEATURES 记"是什么"，本文记"要什么"）。
