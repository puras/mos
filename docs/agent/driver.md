# Agent 循环驱动与运行上下文（LoopDriver / RunContext）

> 来源：`agent/driver/driver.go`（架构 v2 §3.3 / §11.3）。
> 目标读者：理解"智能体循环骨架"的人；新增一种智能体范式（ReAct / PlanExecute / Reflection / Graph / Scripted）的开发者。

---

## 1. 一句话定位

**`driver.go` 是整个 agent 子系统的"骨架合同"**：用 `LoopDriver` 把"循环行为"抽象成可插拔策略，用 `RunContext` 把"循环所需的一切"集中成可配置容器；它本身不实现任何具体循环，仅规定"不管哪种循环，都必须长这个样子、对外提供这些接缝"。

四个具体驱动（`react` / `planexecute` / `reflection` / `scripted`）各自实现 `LoopDriver.Run`，复用同一份 `RunContext` 的会话、工具、钩子、守卫、遥测、审批、检查点。

---

## 2. 三层抽象

```
LoopDriver（接口）── 一种智能体类型 = 一个实现
   │
   ▼
RunContext（结构）── 共享设施容器：会话 / 工具 / 钩子 / 守卫 / 扩展点
   │
   ▼
Agent（结构）── LoopDriver + RunContext 的组合，调用方只用它
```

| 层 | 职责 | 调用方关心程度 |
|---|---|---|
| `LoopDriver` | "怎么转" —— 决策生成、行动编排、循环终止 | 扩展时写新驱动 |
| `RunContext` | "用什么转" —— 会话/工具/钩子/守卫/遥测/审批/检查点 | 装配时一次性配置 |
| `Agent` | "给我跑起来" —— 调 `Run` / `RunStream` | 业务调用方只用这个 |

**好处**：新增智能体类型（如 Graph）只写一个 `LoopDriver.Run` 实现，工具/会话/钩子全部复用，**不污染共享设施**。

---

## 3. 关键设计原则

### 3.1 单调守卫（Guard）vs 可改写钩子（Hook）双轨制

```go
Hooks  *hook.HookBus   // 瀑布钩子（可改写环节）
Guards *hook.GuardBus  // 单调守卫（不可被改写绕过）
```

- **Hook**：可以包装、改写、拦截环节，类比中间件链
- **Guard**：安全闸门，失败即拒绝，**不允许被钩子短路绕过**

`Policy` 字段的注释里特意强调："v2 中 Policy 注入 RunContext，作为单调守卫（GuardBus）的一部分，不可被普通钩子改写绕过。"——这是**纵深防御**思想，避免业务钩子意外把安全检查跳过去。

详见 `docs/agent/hook.md`。

### 3.2 Capability Seam（能力接缝）

```go
Runner     sandbox.Runner     // shell 隔离执行（v2.24 从 tool.Sandbox 拆出）
HTTPClient *http.Client       // 联网工具（httpx.FromContext 取用）
FS         fs.FileSystem      // 文件沙箱（按 Config.FileAccess + Policy 构造）
```

这些字段**全部可为 nil**，驱动内部有兜底逻辑（注释写"nil 时 httpx.Default"/"Sandboxed 或 Local"）。这样：
- **单元测试**：可以注入假实现
- **生产环境**：由平台装配真实现
- **默认场景**：零配置可用

### 3.3 可选扩展点全部"fail-closed"

观察 `Approval`、`Checkpoints`、`Policy`、`ScriptPlanner`、`Reflector` 等字段的注释模式：

> "nil 时对需要确认的操作 fail-closed 拒绝（等价旧版无确认器行为）"
> "ReAct/PlanExecute 驱动不使用，nil 时 reflection 驱动拒绝运行"

**缺能力就拒绝，而不是降级放行** —— 这是安全相关系统的一致原则，避免"忘了配置 = 系统裸奔"。

### 3.4 防御性调用（nil-safe）

```go
func (rt *RunContext) Notify(ev agent.Event) {
    if rt == nil { return }       // 允许 rt 为 nil 调用
    if rt.Observer != nil { ... } // 回调可空
    if rt.Bus != nil { ... }      // 总线可空
}
```

`AddFact` 同样 `if rt == nil { return }` —— **所有方法对 nil 安全**。这种"调用者友好"在 agent 装配链里很值钱，因为很多扩展点可能尚未注入。

---

## 4. 核心数据结构：`RunContext`

按"语义群"分组解读（完整字段见源码 44-112 行）：

### 4.1 基础会话与决策

| 字段 | 说明 |
|---|---|
| `Session *session.Session` | 会话状态、消息历史、事件日志 |
| `Tools *tool.Registry` | 工具注册表 |
| `Planner planner.Planner` | 默认决策器（`planner.NewReAct(model)`） |
| `PlanPlanner planner.PlanPlanner` | PlanExecute 驱动专用；ReAct 不使用 |
| `Reflector planner.Reflector` | Reflection 驱动专用；ReAct 不使用 |
| `ScriptPlanner planner.ScriptPlanner` | Scripted 驱动专用；ReAct 不使用 |

### 4.2 能力接缝

| 字段 | 兜底 |
|---|---|
| `Runner sandbox.Runner` | shell 执行隔离 |
| `HTTPClient *http.Client` | 联网工具，nil → `httpx.Default` |
| `FS fs.FileSystem` | 文件工具，nil 时工具方需自行 fallback |

### 4.3 安全与守卫

| 字段 | 说明 |
|---|---|
| `Policy Policy` | 工具调用安全策略（注入 Guards） |
| `Auditor agent.Auditor` | 工具结果审计门（默认 `NewDefaultAuditor`） |
| `Observer agent.Observer` | 观测回调（emit 投影入口） |
| `Approval approval.Approval` | 异步审批通道 |

### 4.4 提示词组装（M2.6）

| 字段 | 说明 |
|---|---|
| `Assembler agent.SystemAssembler` | 槽位式组装器，**优先于** `SystemPrompt` |
| `SystemPrompt string` | 静态系统引导词（Assembler 为空时使用） |

详见 `docs/agent/system.md`。

### 4.5 运行时控制

| 字段 | 默认 | 说明 |
|---|---|---|
| `ContextBudget int` | 0 = 关闭 | >0 启用上下文利用率监控（`EventContextUsage`） |
| `DoomLoopMaxRepeat int` | 3 | Doom Loop 检测阈值 |
| `MaxTurns int` | 0 = 不限 | 可选保险丝：达到该轮数任务置 blocked |
| `MaxToolParallel int` | 4 | 单轮批量工具并发上限（架构 v2 §3.9） |
| `Stream bool` | false | 是否走流式（增量广播 EventStream） |

### 4.6 外部停止裁决

```go
ShouldStopAfterTurn func(ctx, sess, turn, action) (bool, error)
```

对齐 PI `shouldStopAfterTurn`：每轮结束后回调，返回 `true` 时任务以 `stopped` 收尾。可用于：
- 人工叫停
- 成本控制
- 业务级完成判定

### 4.7 可观测与遥测

```go
Observer  agent.Observer            // 单层回调（API 不变）
Bus       *event.EventBus           // 可订阅事件总线
Telemetry telemetry.TelemetryContext // span 工厂（nil 时 no-op 零开销）
```

`Telemetry` 注入后，driver 回合 / llm 请求 / 工具执行自动打 span（model/provider 属性由平台在适配器层补充 —— 关注点分离）。

### 4.8 可恢复挂起（v2 关键能力）

```go
Approval           approval.Approval
Checkpoints        checkpoint.Store
ResumeCheckpoint   *checkpoint.Checkpoint
```

完整流程见 §6。

---

## 5. 并发模型

```go
factMu sync.Mutex
```

只有一处显式锁，专治一个具体问题：

> 批量工具并发执行时，多个 goroutine 同时审计写 `Session.State.Facts`（`auditHook` 内 `AddFact`）。

**锁收敛到 `RunContext` 而非塞进 `Session`**，因为锁的语义属于"驱动运行时的协调"，不是 Session 自身的不变量。

公开方法：

```go
func (rt *RunContext) AddFact(f session.Fact)
```

`AddFact` 自动加锁 → 调 `Session.State.AddFact` → 解锁。

---

## 6. 流式 vs 非流式：同一驱动，两个开关

```go
func (a *Agent) Run(ctx, sess, input) (string, error) {
    rt.Stream = false
    ...
}
func (a *Agent) RunStream(ctx, sess, input) (string, error) {
    rt.Stream = true
    ...
}
```

只翻转 `rt.Stream` 一个布尔位，驱动内部据此决定是否走 EventStream 增量广播。

**两条不变式**：
1. 模型或规划器不支持流式时**自动回退**为非流式路径，行为与 `Run` 一致（注释明文）
2. **流式是纯增量的增强** —— 不能因为流式导致结果不一样

每次 Run 前后还有一对：

```go
defer agent.FinalizeRunSkills(sess)
agent.ResetRunSkills(sess)
```

这是**每次任务开始前的状态隔离**，避免上一次运行的技能/临时态污染下一次。

---

## 7. 可恢复挂起：Approval + Checkpoint

这一对是 v2 长任务/人工在环场景的标准模式（参考 PI `SuspendedOperation` / LangChain `checkpointer`）：

```
工具调用前 Policy.Allow
   │
   ├─ 通过 → 执行
   │
   └─ 返回 *approval.RequiredError
         │
         ▼
      policyGuard.Allow → Approval.Request
         │
         ├─ Immediate 实现可用 → 直接 resolve 决策，继续
         │
         └─ 无 Immediate → 抛 SuspendError
                │
                ▼
            Checkpoints.Save 挂起点落盘
                │
                ▼
            平台异步 Resolve
                │
                ▼
            Handle.Resume(approval, ckpt)
                │
                ▼
            Driver.Run，ResumeCheckpoint 注入 RunContext
                │
                ▼
            跳过 LLM，直接进入工具批量执行
            按 Decisions[call.ID] 查表放行
```

关键字段协作：

```go
ResumeCheckpoint *checkpoint.Checkpoint  // Resume 时注入的恢复点
```

驱动内部据此决策"从挂起的轮继续执行"还是"重新规划"。**避免 LLM 重新规划浪费 token**，对长任务尤其重要。

详见 `docs/agent/approval.md` 与 `docs/agent/checkpoint.md`。

---

## 8. 观测双通道

```go
Observer  agent.Observer
Bus       *event.EventBus
```

- `Observer`：单层回调，**保持向后兼容**
- `Bus`：可订阅通道，多个订阅者可并行消费
- `Notify(ev)`：同时投递到两者（任一为 nil 自动跳过）

驱动内部统一调 `rt.Notify(...)`，调用方不需要选通道。

---

## 9. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **Hook 可改写、Guard 不可绕过** | `HookBus` / `GuardBus` 双轨 | 纵深防御 |
| **可选扩展点 fail-closed** | `Policy` / `Approval` / `PlanPlanner` 等注释 | 不允许"忘了配 = 裸奔" |
| **能力接缝 nil 可用** | `HTTPClient` / `FS` / `Runner` | 装配友好，零配置可用 |
| **批量工具并发写 Facts 串行化** | `factMu` | 避免数据竞争 |
| **流式是非流式的超集** | `Run` / `RunStream` 共享路径 | 结果一致 |
| **每个会话的 skill/load 只写一次** | `hasSkillLoadEvent(sess)` | 日志即事实（详见 system.md §5.4） |
| **挂起可跨进程恢复** | `ResumeCheckpoint` | 长任务不丢状态 |

---

## 10. 自定义扩展姿势

### 10.1 新增一种智能体类型

实现 `LoopDriver.Run(ctx, rt, input)` 即可。最小骨架：

```go
type MyDriver struct{}

func (d *MyDriver) Run(ctx context.Context, rt *driver.RunContext, input string) (string, error) {
    // rt 内拿会话、工具、planner、hooks、guards、telemetry、approval...
    // 你的循环逻辑
    return result, nil
}

// 使用
a := driver.New(rt, &MyDriver{})
a.Run(ctx, sess, "任务输入")
```

**注意**：四种驱动范式的扩展点（`PlanPlanner` / `Reflector` / `ScriptPlanner`）应**在 RunContext 缺失时 fail-closed**，对齐 `Reflection` 驱动"nil 拒绝运行"的语义。

### 10.2 仅替换系统约束内容

不需要动 `driver`，配 `assistant.Config{SystemPrompt: "..."}` 即可（详见 `docs/agent/system.md` §7）。

### 10.3 替换提示词组装策略

实现 `agent.SystemAssembler`，注入 `RunContext.Assembler`（详见 `docs/agent/system.md`）。

### 10.4 接入新能力接缝

例如新增一个"向量检索能力"：
1. 在 `RunContext` 加 `Retriever memory.Retriever` 字段（已存在示例）
2. 驱动内部 `if rt.Retriever != nil { ... }`，nil 时降级
3. 装配链（`assistant.New`）按需注入

---

## 11. 一句话总结

> **`driver.go` 是 v2 §3.3 列出的四种智能体范式的"骨架合同"**：用策略模式解耦循环行为，用容器集中共享设施，用 Hook/Guard 双轨、可选扩展点 fail-closed、能力接缝 nil 兜底三板斧，兼顾**灵活性、安全性、可观测性、可恢复性**四个维度。

它本身只一个 `.go` 文件却能撑起整个 agent 循环，原因就在于此 —— **只规定接缝，不规定实现**。