# MoS 架构（重建版）

> 配套 [PRD](PRD.md)。旧版架构全文（architecture-v2 + 18 篇子系统文档）在 `main_bak1015` 分支的 `docs/` 下，需要考古时去那里；本文只保留目标形态与必须继承的不变式。

## 1. 分层（目标形态）

```text
装配层    assistant        模型对象 + 工具集 + 运行配置注入，nil 用框架默认（S7）
驱动层    LoopDriver       ReAct 起步；PlanExecute/Reflection/Scripted 按 PRD 逐个回填（S4a ✅）
执行通道  execpath         Policy 守卫 → 批量并发执行 → 审计回填，各驱动共用（S5）
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
| `tool` | schema 生成、Registry、builtin 最小集 | 移植裁剪 | S3 |
| `planner` + `driver` | Planner 接口 + ReAct 决策；LoopDriver + ReActDriver | planner 移植 / 驱动重写 | S4a ✅ |
| `agent`（安全件）/ `hook` / `filesystem` / `sandbox` | Policy、GuardBus、文件 jail、进程隔离 | 移植裁剪 | S5 |
| `agent`（上下文件） | 截断、压缩、Auditor | 重写瘦身 | S6 |
| `assistant` | 一行装配 + 示例 CLI | 重写 | S7 |

### 3.1 循环契约（S4a 定型，S4b 扩展）

一轮循环的事件序列固定为 `turn/start → user → [assistant → tool/call → tool/result]* → turn/end`：

- **历史来源唯一**：`RunContext.History(sess)` = 静态系统引导 + 会话事件投影，驱动不自行维护 `[]Message`；
- **轮次序号派生**：`sess.NextTurn()` 取日志最大值 + 1，Restore 后自然接续；
- **错误必收尾**：任何出错路径都以 `turn/end{reason:error}` 收尾，不留孤儿 turn；
- **能力缺口 fail-closed**：S4a 驱动未接工具执行通道时，模型请求工具调用直接报错（`react.ErrToolsUnsupported`），S4b 接入后放开。

## 4. 治理规则（防再膨胀）

1. 新增一个包 = PRD 有一条 FR 指名要它；否则留在 `main_bak1015` 不动。
2. 钩子/守卫/事件目录预算制：新增一个 HookKind 或事件类型前，先删一个旧的。
3. 每片合入验收门 = `go test ./...` 全绿 + 本文件/README 状态同步 + 该片一页说明，缺一不合。
4. 破坏性变更直接改、不留兼容层，但必须在提交信息与 PRD 变更记录声明。
