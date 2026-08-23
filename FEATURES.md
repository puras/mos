# MoS 功能说明

> 生成日期：2026-08-17 ｜ 对应提交：`5a31e28`（v2.18 M2.8 全量落地后）
> 状态：**M1 ~ M2.8 已完成**；M2.9 / M2.10 / M3 / M4 / M5 规划中
> 本文件按 v2 架构（`docs/architecture-v2.md`）重写，API 以 `README.md`、`docs/architecture-v2.md`、`docs/sdk-contracts.md` 为准。

## 更新记录

| 日期 | 版本 | 更新内容 |
| --- | --- | --- |
| 2026-08-17 | v2.18 | M2.8 全量落地：异步 Approval（`agent/approval`）+ Checkpointer（`agent/checkpoint`）+ AgentHandle/Registry（`agent/handle`）+ TaskState 持久化；Confirmer 改废弃（桥接为 Approval） |
| 2026-08-15 | v2 | 按 v2 架构重写全文件：删除 v1 API（`WithXxx` 选项 / 命令式 run / `agent.New(model, reg, ...)`）描述，改为 RunContext + LoopDriver + HookBus + 事件日志体系；补充 policy 拆分后结构 |

## 1. 项目定位

MoS（Sapient）是基于 Go 的智能体基础框架（Agent Base），提供统一 LLM 接入、声明式工具系统、会话记忆与可插拔规划，帮助快速构建智能体应用。设计理念：**状态写文件不写上下文、只接受环境独立验证过的事实、上下文越少越好**（Harness Engineering / Audit Gate / Loop Engineering）。

MoS 是 SDK，不参与服务化能力范畴：Web UI / API 网关 / 任务调度 / 多租户等 SaaS 平台能力由基于 MoS 的云端智能体平台（MOSSCloud）负责。

## 2. v2 架构总览

```text
装配层：assistant.New（模型对象 + 工具集 + 运行配置注入，nil 用框架默认）    [已实现]
循环层：LoopDriver（ReActDriver）+ HookBus / GuardBus（可改写环节）         [已实现]
基础设施层：EventBus / Session 事件日志 / RunContext / Scope / Observer     [已实现]
现有基础：llm / tool / planner / memory                                     [不动]
```

依赖方向：装配层 → 循环层 → 基础设施层 → 现有基础。现有包不反向依赖新机制。

## 3. 已实现功能

### 3.1 LLM 接入（`llm/`）
- 统一 `ChatModel` 抽象：消息 / 工具调用 / 用量 / 流式输出 / `FinishReason`，模型对象注入，零第三方依赖
- 协议适配器：OpenAI 兼容（DeepSeek / 通义千问 / GLM / Ollama 等）、Anthropic Claude、Google Gemini，均支持函数调用与流式输出；`FinishReason` 三协议透传（截断 fail-closed 的基础）
- 协议注册表 `Registry` + `Factory`，按需注册协议；`Capability` 能力位（如流式 / 视觉）
- 装饰器链 `llm/wrap`：`WithLogging` / `WithRetry` / `WithTimeout`

### 3.2 工具系统（`tool/`）
- 声明式工具：结构体 + tag 自动生成 JSON Schema，参数自动校验解析（`tool.Parse`）
- `Registry` 注册 / 查询 / 统一执行；`Call` 入口带参数容错（拼接 JSON 合并为单一对象）
- 内置工具包 `tool/builtin`：shell / current_time / web_fetch / read_file / write_file / find_files / grep，纯标准库
- 扩展工具包 `tool/extension`：shell（沙箱受限执行）、web_search（DuckDuckGo / Tavily / Serper / Brave / SearXNG）
- 工具输出截断（M2.3）：行数 / 字节双重限制 + 按工具类型智能截断 + 逃生通道（完整结果落盘、提示模型按偏移回读）
- 沙箱 `tool.Sandbox`：工作目录 jail + 超时 + 输出上限 + 进程资源限制 + 环境裁剪 + 网络控制

### 3.3 Agent 核心（`agent/`，v2 重构落地）
- **事件总线 `EventBus`**：四调度模式（emit 观察 / waterfall 改写 / parallel 并行 / serial 裁决），事件目录集中在 `agent/events.go`
- **钩子与守卫**：`HookBus`（waterfall 语义：next 委托、可短路、可替换）承载 pre-step / tool pre-execute / post-execute 等可改写环节；`GuardBus`（serial 裁决）承载策略与单调守卫（DoomLoopGuard）
- **循环可替换 `LoopDriver`**：`ReActDriver` 已实现；截断工具调用 fail-closed（对齐 PI `failToolCallsFromTruncatedMessage`）
- **`RunContext`**：运行期上下文，框架概念（Planner / Auditor / Policy / Compactor / Retriever / Observer）由调用方注入，nil 用默认
- **会话事件日志 `Session.Events`**：append-only 事件日志是唯一事实来源，消息历史由 `deriveMessages` 投影派生；事件携带 Turn 序号，支持流式端到端输出
- **会话日志持久化**：`session.Persistence` 后端接口（backend 可换）+ `JSONL` 实现（每会话一文件、追加写、容忍尾部半行）；`Session.Append` 同步落盘 fail-fast；`Restore` 重放重建 + 孤儿 turn 合成 `interrupted`（崩溃恢复，不截断）
- **`agent/scope` 注册作用域**：per-agent/per-session 回收容器，按注册逆序回滚（`Close` 幂等），`On` 返回可单独撤销的 disposer（对齐 Cordis registrations are reversible）
- **批量工具执行（并发）**：`agent/executor` 三阶段（preflight 串行决策 → 有界并发 → 按源序回填，对齐 PI `executeToolCallsParallel`）；守卫拦截集合显式失败回填；`tool.SequentialTool` 整批串行回退；`MaxToolParallel` 并发上限；审计经 `AddFact` 串行化
- **脚本化编排（PTC）**：`agent/driver/scripted`——模型产依赖 DAG 脚本 → 分波有界并发执行（纯工具调用）→ 合成最终回复（共 2 次模型调用）；脚本校验 fail-closed（依赖环 / 工具存在 / 参数名 / 步数上限）；M2.8 审批断点（checkpoint 携带 ScriptState）；复用 execpath 共享执行通道
- **Telemetry seam**：`agent/telemetry` 显式 span 契约（TelemetryContext/TelemetrySpan + Noop + WithSpan 回调生命周期，对齐 PI pi-telemetry）；`RunContext.Telemetry` 注入后驱动自动打 `driver.turn` / `llm.request` / `tool.execute` span；`Recorder` 内存参考实现 + `Conformance` 契约测试（平台适配器复用）
- **异步 Approval（M2.8）**：`agent/approval`——`Approval` 接口（Request/Resolve）+ `SuspendError` 驱动挂起 + `InMemApproval`（内存通道）+ `ConfirmerAdapter`（旧同步确认器桥接，`Immediate` 接口保留 CLI y/N）；策略检查器对需确认操作返回 `*approval.RequiredError`，驱动 `policyGuard` 发起审批、批量 preflight 守卫先行日志后置、挂起整批中止（fail-closed：未配置 Approval 时需确认操作拒绝）
- **Checkpoint 断点恢复（M2.8）**：`agent/checkpoint`——`Checkpoint` 快照（挂起轮工具批 + 审批请求 + TaskState 快照）+ `Store` seam（内存 / 文件默认后端，sqlite 等数据库由业务层注入）；`AgentHandle.Resume(decision)` 注入决策表从挂起轮续跑；`approval/request|resolve`、`checkpoint/save|restore` 事件落会话日志（durable 审计）
- **AgentHandle / Registry（M2.8）**：`agent/handle`——create/resume/dispose 生命周期句柄 + 会话级注册表（对齐 dsh AgentRegistry）；`Resume` = 会话重放 + checkpoint 挂载 + 资源校验（`ValidateMissing`：会话事件引用的工具须存在于 RunContext.Tools）；`assistant.CreateHandle` / `assistant.Handle(sessionID)` 装配入口；`session.Persistence.SaveState/LoadState` 持久化 TaskState（目标 / 已验证事实 / 状态机）
- **`request_human_input` 工具（M2.8）**：模型主动请求人类输入/确认/授权——配置 Approval 时自动注册；首次调用经 `Approval.Request` 挂起（checkpoint 落库），人类在平台作答（`ApprovalDecision.Content`），`Handle.Resume` 续跑后工具返回回答给模型；拒绝时原因回填模型；`assistant.RequestHumanInput` 亦可显式加入工具集
- **审计式任务状态（M2.1 + M2.2）**：`TaskState`（目标 / 已验证事实 / 状态机）独立于消息历史；`Auditor` 审计门只接受环境可验证事实（默认白名单只读工具，可 `Trust` 扩展），模型自述不算数
- **fresh-context 压缩（M2.5）**：超 `KeepRecent` 触发，不切断 ToolCall-Result 对；摘要直接采用 `TaskState.Snapshot()`
- **Doom Loop 检测（M2.5）**：相同工具 + 参数连续 N 次 → 单调守卫拦截（`DoomLoopGuard`），上下文利用率 40% 告警线事件广播
- **系统提示词动态组装（M2.6）**：槽位化（系统约束 / 任务定义 / 当前状态 / 工具签名 / 项目指令 / 技能清单 / 历史摘要）；CLAUDE.md 向上递归、就近覆盖；Skills 懒加载（SKILL.md frontmatter 清单，正文按需读取）
- **RAG 注入（M2.7，扩展点）**：`memory.Retriever` 接口 + 每轮按 Goal 检索注入，知识守门（源验证 / 冲突裁决 / 知识差距检测 / MaxChunks 截断），注入只作用于当轮发送副本、不进历史；实现由使用方提供（原 `memory/longterm` 已移除）

### 3.4 决策规划（`planner/`）
- `Planner` / `StreamingPlanner` 接口可插拔，`ReAct` 默认实现（`planner.NewReAct(model)`）
- 装饰器：`WithLogging` / `WithRetry`

### 3.5 长期记忆（`memory/`）
- `memory.Memory`（会话记忆接口，预留）+ `memory.Retriever`（RAG 检索扩展点，agent 每轮注入钩子保留）
- 原 `memory/longterm`（SQLite + FTS5 + 向量）实现已移除（2026-08 精简：参考 DSH/PI 均未内置 RAG），实现由使用方按需提供
- 装饰器 `memory/wrap`：`WithLogging` / `WithRetry`

### 3.6 安全防护（M1.5 + M1.6 + M2.8 雏形）
- `driver.Policy` 执行前拦截：`assistant.DefaultPolicy` 按职责拆分为三文件——`policy.go`（策略核心：Allow / Register / 检查器装配）、`policy_shell.go`（shell 命令分级校验）、`policy_file.go`（路径校验）
  - 绝对危险命令（shutdown / dd / mkfs / fork 炸弹 / 写块设备）无条件拒绝
  - 受限危险命令（rm / sudo / chmod / mv / kill / 解释器 `-c/-e` 入口 / find 删除语义）须 WorkDir 内 + 人工确认
  - 路径越界防护（withinDir）+ 命令前缀白名单 + fail-closed（未知工具默认拒绝）
- `Sandbox` 执行中隔离：工作目录 jail + 超时 + 输出上限 + 进程资源限制 + 环境裁剪 + 网络控制
- 异步人工确认（M2.8）：`agent/approval`——需确认操作由策略返回 `*approval.RequiredError`，驱动 `policyGuard` 发起 `Approval.Request` → `SuspendError` 挂起 → 平台 `Resolve` → `AgentHandle.Resume` 续跑（one-shot，默认拒绝 fail-closed）；旧 `Confirmer`（CLI y/N）经 `approval.NewConfirmerAdapter` 桥接保留（`Immediate` 立即决策，不挂起）

### 3.7 组装层（`assistant/`）
- `assistant.New`：注入模型对象 + 工具集 + 运行配置（`assistant.Config`），nil 时用框架默认（策略 / 规划器 / 审计器 / 记忆 / 观察）
- `DefaultPolicy`：默认安全策略（shell 白名单 + WorkDir 限制 + 异步 Approval 人工确认）
- `Run` / `RunStream` 统一入口；`NewSession` 按 UUID 创建独立会话（配置 Persistence 时自动挂载）
- `CreateHandle` / `Handle(sessionID)`：M2.8 会话句柄（挂起-恢复 + 跨进程 Resume）

## 4. 架构组成

| 包 | 职责 |
| --- | --- |
| `llm/` | 模型抽象、OpenAI / Anthropic / Gemini 适配器、注册表、装饰器 |
| `tool/` | 工具注册与执行、内置 / 扩展工具包、沙箱 |
| `planner/` | Planner 接口与 ReAct 默认实现、装饰器 |
| `agent/` | v2 核心：EventBus / HookBus / GuardBus / LoopDriver / Session 事件日志 / 审计 / 状态 / 压缩 / DoomLoop / 系统提示词 / RAG / 截断 |
| `agent/approval/` | 异步审批（M2.8）：Approval 接口 / SuspendError / InMem / ConfirmerAdapter |
| `agent/checkpoint/` | 断点快照 + Store seam（M2.8）：内存 / 文件后端 |
| `agent/handle/` | AgentHandle / Registry 生命周期（M2.8）：create / resume / dispose + 资源校验 |
| `memory/` | 记忆接口（Memory + Retriever 扩展点）、装饰器 |
| `assistant/` | 组装层、默认策略（policy 三文件）、异步审批装配、会话句柄入口 |

SDK 零第三方依赖（仅依赖本地协议库 `hegq.net/mogo`）。

## 5. 完整性评估

### 5.1 对照路线图
- ✅ **已完成（11 项）**：M1、M1.5、M1.6、M2.1 ~ M2.8；v2 架构（EventBus / HookBus / LoopDriver / Session 事件日志）+ M2.8 异步审批 / Checkpointer / AgentHandle 已落地
- ❌ **未开始（5 项）**：M2.9 经验记忆、M2.10 工具规模化、M3 多智能体编排、M4 生产化、M5 自我进化

### 5.2 质量现状
- 测试：SDK 全部测试通过（agent / assistant / tool / planner / llm 等），含 llm 适配器 httptest 协议模拟；M2.8 新增 approval / checkpoint / handle / session 状态持久化 / 端到端挂起-恢复（含拒绝路径）测试

### 5.3 已知问题与风险
- **v2 重构全部落地**：LoopDriver 生态（ReAct / Plan&Execute / Reflection + DriverRouter）+ SeamRegistry + 声明式配置（YAML）+ Scope + 会话持久化（JSONL）+ ConcurrentExecutor 批量执行 + Telemetry seam + M2.8（Approval / Checkpoint / AgentHandle）已全部实现
- **待办缺口**：token 预算上限、上下文重置与跨 Session handoff、模型分级路由、异常恢复、可恢复运行目录（M4）等仍为空
- **M2.8 后续**：Approval TTL 自动超时、checkpoint 加密/签名、多句柄并发恢复（业务层职责）
- **路线图条目**：M2.9 / M2.10 / M3 条目内容详实但均为规划，实现前需按实际落地情况修订

## 6. 快速验证

```bash
go test ./...
```
