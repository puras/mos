# MoS 架构（重建版）

> 配套 [PRD](PRD.md)。旧版架构全文（architecture-v2 + 18 篇子系统文档）在 `main_bak1015` 分支的 `docs/` 下，需要考古时去那里；本文只保留目标形态与必须继承的不变式。

## 1. 分层（目标形态）

```text
装配层    assistant        模型对象 + 工具集 + 运行配置注入，nil 用框架默认（S7）
驱动层    LoopDriver       ReAct 起步；PlanExecute/Reflection/Scripted 按 PRD 逐个回填（S4a ✅）
执行通道  execpath         守卫裁决 → 调度执行 → 结果回填，各驱动共用（S5a 骨架 ✅ / S5e 补齐）
基础设施  session/event/hook  事件日志会话 + 总线 + 钩子守卫（S1/S5）
能力层    llm / tool / filesystem / sandbox   协议适配、工具、文件 jail、进程隔离（S2/S3/S5）
协议层    moproto          外部本地库（只读，见 AGENTS.md）
```

依赖方向自上而下，禁止反向引用。唯一例外：`llm` 的核心值类型（`Message` / `ToolCall` / `Usage`）是全框架共享的对话词汇表（纯定义、零依赖），基础设施层（session 等）可直接引用——日志与协议同形，避免翻译边界丢字段；禁止的是能力层/驱动层反向依赖上层。每层在本文件登记后才允许新增包。

## 2. 不变式（从旧实现继承，任何切片不得违背）

1. **fail-closed**：未知工具拒绝；capability 缺失即 `NotConfiguredError`；必需组件（规划器/评审器）缺失拒绝运行而非降级放行；截断消息中的工具调用 fail-closed。
2. **日志即事实**：会话日志 append-only；`Append` 同步落盘 fail-fast（写失败事件不进内存）；崩溃恢复只补事件不改写（孤儿 turn 合成 `interrupted`）。
3. **审计不变式**：只有环境可验证的事实进入 `TaskState`，模型自述不算数；机器可回放信号（退出码/路径命中/事件日志）优先于 LLM 判断；有界轮次耗尽诚实停机，不造假绿灯。
4. **上下文是稀缺资源**：截断 + 压缩；压缩切割点不切断 ToolCall-Result 对；压缩摘要 = `TaskState` 快照而非 LLM 重新总结。
5. **Hook/Guard 双轨制**：可改写环节走 HookBus（waterfall，可短路/改写/替换）；安全约束走 GuardBus（serial，单调裁决，不可被钩子改写绕过）。
6. **零开销降级**：扩展点 nil 时行为与未注入完全一致（Telemetry/Observer/EventBus 等 no-op 零成本）。
7. **依赖纪律**：SDK 近零依赖（协议层 + uuid + yaml.v3）；协议仓库只读；不操作数据库（`Persistence` 接口 + 轻量默认，数据库后端由业务层注入）。

## 3. 包地图（随切片生长，勿提前创建）

| 包 | 职责 | 来源 | 切片 |
| --- | --- | --- | --- |
| `session` | 会话事件日志、消息投影、TaskState、JSONL 持久化 | 重写瘦身 | S1 |
| `llm`（+`llm/transport`） | ChatModel 抽象、协议适配、共享 HTTP envelope | 移植归档 | S2 |
| `tool`（+`tool/builtin`） | 工具接口、注册表、Schema 生成；`current_time` 起步 | 移植裁剪 | S3 ✅ |
| `planner` + `driver` | Planner 接口 + ReAct 决策；LoopDriver + ReActDriver | planner 移植 / 驱动重写 | S4a ✅ |
| `agent/execpath` | 工具调用统一执行通道：preflight 裁决 + 调度 + 按调用序回填 | 重写 | S5a ✅ |
| `agent/hook` + `agent`（安全件）/ `filesystem` / `sandbox` | HookBus/GuardBus 双轨制、Policy、文件 jail、进程隔离 | 移植裁剪 | S5b/S5c/S5d/S5f |
| `agent`（上下文件） | 截断、压缩、Auditor | 重写瘦身 | S6 |
| `assistant` | 一行装配 + 示例 CLI | 重写 | S7 |

> S3 边界：`tool/builtin` 只带 `current_time`（无安全副作用）。涉及文件与命令执行的
> 内置工具（read_file / write_file / find_files / grep / shell）等 S5 的 filesystem jail
> 与 sandbox 就位后再加入——不先发一版无防护的实现、再返工加锁。

### 3.1 循环契约（S4a 定型，S4b 落地工具循环）

一个 turn 的事件序列固定为 `turn/start → user → (assistant → tool/call* → tool/result*)* → assistant → turn/end`：

- **历史来源唯一**：`RunContext.History(sess)` = 静态系统引导 + 会话事件投影，驱动不自行维护 `[]Message`；工具定义单独传决策器（对话与能力是两件事）；
- **轮次序号派生**：`sess.NextTurn()` 取日志最大值 + 1，Restore 后自然接续；
- **投影合法性不变式**：assistant 的 `ToolCalls` 与随后的 `tool/result` 按 CallID 成对——否则投出的历史对服务商非法（缺 tool 消息会报错）；
- **日志顺序**：`tool/call` 按模型顺序全部先落，执行后 `tool/result` 按序回填，Seq 连续可重放（为并发执行留形，S4b 目前串行）；
- **错误分级**：工具报错（含模型幻觉出的未注册工具）是**结果**，以 `IsError` 回填给模型让它自我纠正，不中断 turn；只有落盘失败与决策失败才中断，且都以 `turn/end{reason:error}` 收尾，不留孤儿 turn；
- **能力缺口 fail-closed**：未装 `RunContext.Tools` 却要求工具时直接报 `react.ErrToolsUnsupported`，且**不写 assistant 事件**（避免留下非法历史）；
- **内层上限**：单 turn 决策上限 `maxDecisionRounds = 16`，防模型空转；S6 的 DoomLoopGuard 与 MaxTurns 落地后由其接管停止裁决，届时重议。

### 3.2 执行通道契约（S5a 骨架，S5e 补齐）

工具执行的语义全库只在 `agent/execpath` 一处，驱动只负责投影与收尾：

```text
ExecuteBatch(ctx, reg, calls) []Outcome   // 结果与 calls 严格同序

preflight（裁决）  S5a：空实现恒通过          S5b/S5e：GuardBus 串行裁决
execute  （执行）  S5a：按调用序串行          S5e：有界并发，SequentialTool 批内串行
finalize （回填）  调用方（驱动）落 tool/result 事件
```

- **语义中立于驱动**：不感知 ReAct / 未来的 PlanExecute、Scripted，也不感知 turn 编号与会话；它只回答「这批调用各得什么结果」。
- **回填顺序 = 调用顺序**：结果按模型给定顺序排列而非完成顺序，并携带原 `CallID`，调用方靠它对回每个调用——并发化后此不变式尤其重要。
- **错误即结果**：工具报错、参数非法、模型幻觉出的未注册工具名，全部转成 `Outcome{IsError: true}` 而非返回 error——那是「本次调用失败」不是「系统故障」，回填给模型让它自我纠正。中断 turn 的只有落盘失败与决策失败。
- **fail-closed 兜底**：注册表为 nil 时整批判失败，不 panic（正常路径由驱动提前拦下，这里是防御性底线）。
- **不落日志、不裁断、不重试**：日志语义属 `session` 与驱动，避免执行语义与日志语义纠缠。
- **待定项（S5e）**：归档实现的 preflight 裁决**先于** `tool/call` 日志（挂起时不留痕），而 §3.1 现约定是「全部先落再执行」。两者不能同时成立，S5e 开工时二选一并同步改写 §3.1。

## 4. 治理规则（防再膨胀）

1. 新增一个包 = PRD 有一条 FR 指名要它；否则留在 `main_bak1015` 不动。
2. 钩子/守卫/事件目录预算制：新增一个 HookKind 或事件类型前，先删一个旧的。
3. 每片合入验收门 = `go test ./...` 全绿 + 本文件/README 状态同步 + 该片一页说明，缺一不合。
4. 破坏性变更直接改、不留兼容层，但必须在提交信息与 PRD 变更记录声明。
