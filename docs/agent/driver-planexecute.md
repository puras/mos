# PlanExecuteDriver —— 计划-执行循环驱动

> 来源：`agent/driver/planexecute/planexecute.go`（架构 v2 §3.3，Plan-and-Solve 模式）。
> 目标读者：理解"先规划再执行"循环的所有人；想自定义规划器 / 校验函数 / 步骤执行器的开发者。

---

## 1. 一句话定位

**`PlanExecuteDriver` 是 MoS 的"写计划 → 逐条执行 → 校验交付物"循环驱动**：生成 `Plan{Summary: 步骤}` 后**每步内嵌 ReActDriver 执行**（共享同一 `RunContext`），最后调 `VerifyFunc` 校验交付物，**全部通过才收尾**。

源码顶端的注释（planexecute.go:1-5）一段话讲清定位：

> "Package planexecute 提供 Plan&Execute 循环驱动（架构 v2 §3.3，Plan-and-Solve 模式）：**写计划 → 逐条执行（内嵌 ReActDriver 作为执行器）→ 校验交付物**。与 ReActDriver 的关系：本包是同一 RunContext 上的另一种 LoopDriver 实现，**共享会话、工具、钩子、守卫与审计**；执行阶段复用 react 驱动逐条执行计划步骤。"

文件只有一个 `planexecute.go`（192 行），加测试 1 个文件。

---

## 2. 数据结构 + 三个 Option

```go
type PlanExecuteDriver struct {
    executor driver.LoopDriver   // 步骤执行器（默认 ReActDriver）
    planner  planner.PlanPlanner
    verify   VerifyFunc
}

type Option func(*PlanExecuteDriver)
```

| Option | 默认 | 用途 |
|---|---|---|
| `WithExecutor(driver.LoopDriver)` | `&react.ReActDriver{}` + `react.NewReAct(rt)` | 替换步骤执行器 |
| `WithPlanner(planner.PlanPlanner)` | `rt.PlanPlanner` | 注入规划器（nil 时单步退化） |
| `WithVerifier(VerifyFunc)` | nil | 注入校验回调（nil 时默认通过） |

**`VerifyFunc` 签名**（planexecute.go:25）：

```go
type VerifyFunc func(ctx context.Context, rt *driver.RunContext, plan planner.Plan, executed []planner.PlanStep) (bool, string)
```

返回 `(false, "原因")` 时任务以 `blocked` 收尾（plan/end failed）。

---

## 3. `New` 装配（planexecute.go:55-66）

```go
func New(rt *driver.RunContext, opts ...Option) *driver.Agent {
    d := &PlanExecuteDriver{}
    for _, o := range opts { o(d) }
    if d.executor == nil {
        d.executor = &react.ReActDriver{}
        react.NewReAct(rt)   // ← 关键：补全 rt 默认钩子
    }
    return driver.New(rt, d)
}
```

**关键设计点**（注释 planexecute.go:53-54）：

> "默认钩子/守卫装配与 react.New 一致（**同一 rt 只应由一个驱动入口装配**，避免默认钩子重复注册）"

—— 当业务层用 `WithExecutor` 注入了自定义执行器时（不会再调 `react.NewReAct`），需要自己保证 rt 默认钩子已装。Router 路径会用裸实例惰性装配做兜底（见 §4）。

---

## 4. `Run` 主循环（planexecute.go:69-146）

### 4.1 五段式流程

```
┌─ 初始化
│   ├─ 检查 rt.Session / rt.Tools
│   ├─ 裸实例惰性装配（d.executor == nil 时补 ReActDriver）
│   ├─ sandbox.WithRunner 注入 runner
│   └─ TaskState.Goal / Status 初始化
├─ turn = sess.NextTurn() + 写 turn/start + user/message
├─ 计划阶段
│   ├─ makePlan（planner 或单步退化）
│   ├─ 写 plan/start 事件 + 广播 EventPlan
│   └─ 写每个 plan/step pending 事件
├─ 执行阶段
│   └─ for i := range plan.Steps:
│       ├─ ctx.Err() 检查
│       ├─ 写 plan/step running 事件
│       ├─ out, err := d.executor.Run(ctx, rt, step.Summary)
│       └─ 写 plan/step done 事件 + last = out
├─ 校验阶段
│   └─ d.verify(ctx, rt, plan, plan.Steps)
│       ├─ ok=true：继续
│       └─ ok=false：d.fail(...) blocked 收尾
└─ 收尾
    ├─ 写 plan/end completed 事件
    ├─ TaskState.Status = StatusCompleted
    ├─ 写 turn/end 事件
    └─ rt.Notify(EventDone) + return last, nil
```

### 4.2 裸实例惰性装配（planexecute.go:78-80）

```go
if d.executor == nil {
    d.executor = &react.ReActDriver{}
}
```

**为什么需要兜底？** Router（`agent/router`）会裸构造 `PlanExecuteDriver`（不调 `New`）—— 此时 d.executor 是 nil。注释（planexecute.go:76-77）：

> "裸实例惰性装配：Router 等场景直接构造 PlanExecuteDriver 时补齐内嵌执行器（**ReActDriver 的 Run 会完成默认钩子/守卫装配**）"

### 4.3 任务回合由外层开启（planexecute.go:92-98）

```go
turn := sess.NextTurn()
_ = sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: turn, Reason: "user"})
_ = sess.AppendUserMessage(input, "user")
```

—— **与 Reflection 不同**：reflection 的 turn 由内嵌 ReAct 开（注释 planexecute.go vs reflection.go 对照看）；planexecute 由外层开，每条 plan step 是同一个 Turn 的子任务。

### 4.4 执行每步（planexecute.go:115-131）

```go
for i := range plan.Steps {
    step := plan.Steps[i]
    if err := ctx.Err(); err != nil {
        return d.fail(ctx, sess, turn, step, err)
    }
    _ = sess.Append(event.SessionEventPlanStep, event.PlanStepData{
        Turn: turn, Index: step.Index, Summary: step.Summary, Status: "running",
    })
    out, runErr := d.executor.Run(ctx, rt, step.Summary)
    if runErr != nil {
        return d.fail(ctx, sess, turn, step, runErr)
    }
    _ = sess.Append(event.SessionEventPlanStep, event.PlanStepData{
        Turn: turn, Index: step.Index, Summary: step.Summary, Status: "done",
    })
    last = out
}
```

**关键设计点**：

| 设计 | 含义 |
|---|---|
| **每步 ctx.Err() 检查** | 计划可长跑（每步是子任务），ctx 取消快速 fail |
| **`step.Summary` 作为内嵌执行的输入** | 每步独立问题，ReAct 自行决定工具调用 |
| **`last = out`** | 最后一步输出作为整个 Run 的返回值 |
| **同 Turn 多步** | 所有 plan step 共用 planexecute 开的 turn（不递增） |

### 4.5 `fail` 收尾（planexecute.go:171-181）

```go
func (d *PlanExecuteDriver) fail(ctx context.Context, sess *session.Session, turn int, step planner.PlanStep, err error) (string, error) {
    _ = sess.Append(event.SessionEventPlanStep, event.PlanStepData{
        Turn: turn, Index: step.Index, Summary: step.Summary, Status: "failed",
    })
    _ = sess.Append(event.SessionEventPlanEnd, event.PlanEndData{Turn: turn, Status: "failed", Detail: err.Error()})
    sess.State.Status = session.StatusBlocked
    if step.Index > 0 {
        return "", fmt.Errorf("agent: plan step %d (%s): %w", step.Index, step.Summary, err)
    }
    return "", err
}
```

**关键设计点**：

| 设计 | 含义 |
|---|---|
| **`plan/end` 写 failed + Detail** | 审计 / 复盘能看到失败原因 |
| **TaskState.Status = StatusBlocked** | 后续任务不允许再跑（会话进入"被阻"态） |
| **Index > 0 才包 step 信息** | Index=0 是 verify 失败，错误信息已在 verify 内部 |

---

## 5. `makePlan` 计划生成（planexecute.go:149-168）

### 5.1 三层优先级

```go
func (d *PlanExecuteDriver) makePlan(ctx, rt, sess, input) (planner.Plan, error) {
    pp := d.planner
    if pp == nil { pp = rt.PlanPlanner }
    if pp == nil {
        return planner.Plan{Goal: input, Steps: []planner.PlanStep{{Index: 1, Summary: input}}}, nil
    }
    plan, err := pp.Plan(ctx, input, session.ConvertToLlm(sess.Messages()))
    ...
}
```

| 优先级 | 规划器 | 退化 |
|---|---|---|
| 1 | `WithPlanner(p)` | — |
| 2 | `rt.PlanPlanner` | — |
| 3 | nil | **单步退化**（整个 input 作为单步） |

**单步退化**（planexecute.go:155）：把整个 input 当作 1 个 step，让内嵌 ReAct 自由发挥 —— **不配置规划器也能跑**（与 plan-execute 同源退化语义）。

### 5.2 三处容错

```go
if plan.Goal == "" {
    plan.Goal = input
}
if len(plan.Steps) == 0 {
    plan.Steps = []planner.PlanStep{{Index: 1, Summary: input}}
}
```

—— 规划器返回的 Plan 缺 Goal/Steps 时**显式兜底**，避免后续逻辑崩溃（fail-closed 但宽容输入）。

---

## 6. 计划事件 + 观测

每次 Run 写入的事件链：

```
SessionEventTurnStart{Turn, Reason: "user"}
SessionEventUserMessage
SessionEventPlanStart{Turn, Goal, Steps: N}
   ├─ EventPlan 通知（rt.Notify）
   └─ N × SessionEventPlanStep{Index, Summary, Status: "pending"}
N × (per step):
   ├─ SessionEventPlanStep{Status: "running"}
   ├─ ...（内嵌 ReAct 写 turn/start, user/message, llm/request, tool/result, turn/end 等）
   └─ SessionEventPlanStep{Status: "done"}
SessionEventPlanEnd{Turn, Status: "completed"}
SessionEventTurnEnd{Turn, Reason: "done"}
   └─ EventDone 通知
```

**观测内容**：

| 字段 | 含义 |
|---|---|
| `EventPlan.Content` | 计划文本（"计划（N 步）：1. xxx 2. xxx"） |
| `EventPlanStep.Index` | 步骤序号（1-based） |
| `EventPlanStep.Status` | pending / running / done / failed |
| `EventPlanEnd.Status` | completed / failed |
| `EventPlanEnd.Detail` | failed 时记 err.Error() |

---

## 7. 与 ReActDriver 的关系

| 维度 | ReActDriver | PlanExecuteDriver |
|---|---|---|
| 入口 | `assistant.New(...)` 默认 react | `assistant.New(...)` `Driver="plan-execute"` |
| 任务循环 | 单轮循环 | 写计划 + 多步循环 |
| 步骤执行 | 自身 | 内嵌 ReActDriver（**默认**） |
| 共享 RT | ✅ | ✅（同一 rt） |
| 共享钩子 | ✅ | ✅（默认钩子装配与 react.New 一致） |
| 规划器 | `rt.Planner`（决策） | `rt.PlanPlanner`（计划） |
| 评审器 | — | — |
| 校验器 | — | `WithVerifier(VerifyFunc)` |

**关键不变量**：

| 不变量 | 实现 | 含义 |
|---|---|---|
| **同 rt 只装一次默认钩子** | New 注释 + `react.NewReAct(rt)` | 避免重复注册 |
| **内嵌 ReAct 完成默认钩子装配** | `executor == nil` 分支 | router 路径兜底 |
| **同 Turn 多步** | planexecute 开 turn，inner ReAct 不递增 | 任务级粒度 |

**多步执行链路示例**：

```
PlanExecuteDriver.Run(ctx, rt, "把 README.md 翻译成英文并提交")
   ├─ makePlan → Plan{Steps: [{1, "读取 README.md"}, {2, "翻译成英文"}, {3, "写入 README.en.md"}, {4, "git commit"}]}
   ├─ executor.Run(ctx, rt, "读取 README.md")
   │     └─ ReActDriver.Run → 调 read_file → 返回内容
   ├─ executor.Run(ctx, rt, "翻译成英文")
   │     └─ ReActDriver.Run → 调 llm → 返回翻译文本
   ├─ executor.Run(ctx, rt, "写入 README.en.md")
   │     └─ ReActDriver.Run → 调 write_file → 触发 Approval（如配置）→ 通过 → 写入
   └─ executor.Run(ctx, rt, "git commit")
         └─ ReActDriver.Run → 调 shell（受限危险）→ Approval 通过 → commit
```

---

## 8. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **同 rt 默认钩子只装一次** | New 注释 + `react.NewReAct(rt)` | 避免重复 |
| **裸实例惰性装配** | Run 顶部 `executor == nil` 检查 | router 兜底 |
| **plan → executor 失败立即收尾** | Run 内 `runErr != nil { return fail }` | 不继续后续步骤 |
| **plan/end 必写** | fail + 正常路径 | 事件链完整 |
| **verify 默认通过** | `if d.verify != nil` | 零配置可用 |
| **planner 缺 → 单步退化** | makePlan 第 3 层 | 容错 |
| **Plan.Goal 缺 → input 兜底** | makePlan 容错 | 容错 |
| **ctx 取消快速 fail** | 每步前 `ctx.Err()` | 长跑可中断 |
| **last = out** | for 循环末尾 | 最后一步为返回值 |
| **同 Turn 不递增** | planexecute 开 turn | 任务级粒度 |

---

## 9. 自定义扩展姿势

### 9.1 注入自定义规划器（基于 LLM 的 plan-and-solve）

```go
type LLMPlanPlanner struct{}

func (p *LLMPlanPlanner) Plan(ctx context.Context, goal string, msgs []llm.Message) (planner.Plan, error) {
    resp, err := llm.Generate(ctx, plannerPrompt(goal))
    if err != nil { return planner.Plan{}, err }
    return parsePlan(resp), nil  // 解析为 Plan{Goal, Steps}
}

agent, _ := assistant.New(model, tools, assistant.Config{
    Driver: "plan-execute",
    PlanPlanner: &LLMPlanPlanner{},
    FileAccess: ...,
})
```

### 9.2 注入校验回调（产品级验收）

```go
verifier := func(ctx context.Context, rt *driver.RunContext, plan planner.Plan, executed []planner.PlanStep) (bool, string) {
    // 检查 TaskState.Facts 是否包含 "translation_done" 等业务事实
    facts := sess.State.Facts
    hasTranslation := false
    for _, f := range facts {
        if strings.Contains(f.Summary, "translate") {
            hasTranslation = true
            break
        }
    }
    if !hasTranslation {
        return false, "missing translation fact"
    }
    return true, ""
}

agent, _ := assistant.New(model, tools, assistant.Config{
    Driver: "plan-execute",
    PlanPlanner: &LLMPlanPlanner{},
    // 装配时定制 verifier 通过自定义 DriverFactory
})
```

或直接用 `DriverFactory`：

```go
agent, _ := assistant.New(model, tools, assistant.Config{
    DriverFactory: func(rt *driver.RunContext) *driver.Agent {
        return planexecute.New(rt, planexecute.WithVerifier(verifier))
    },
})
```

### 9.3 注入自定义步骤执行器（嵌套 plan-execute）

```go
// 每步也是 plan-execute（递归规划）
inner := planexecute.New(rt)

outer := planexecute.New(rt, planexecute.WithExecutor(inner))

agent, _ := assistant.New(model, tools, assistant.Config{
    DriverFactory: func(rt *driver.RunContext) *driver.Agent {
        return outer
    },
})
```

### 9.4 单步退化模式（不配规划器）

```go
agent, _ := assistant.New(model, tools, assistant.Config{
    Driver: "plan-execute",  // 无 PlanPlanner → 单步
})
// 等价于 ReActDriver（只是事件链多一层 plan/start/end）
```

---

## 10. 一句话总结

**`PlanExecuteDriver` 是 MoS 的"计划-执行"循环驱动：写计划（planner/单步退化）→ 逐条内嵌 ReActDriver 执行（每步一个子任务，共用外层 Turn）→ 校验交付物（可选 VerifyFunc）→ 失败 fail-closed 收尾（TaskState.Blocked + plan/end failed）。** 与 ReActDriver 共享同一 RunContext 的钩子/守卫/审计，默认钩子装配与 react.New 一致（同 rt 只装一次），校验回调可选（默认通过）让零配置也能跑 —— 是 v2 §3.3 Plan-and-Solve 模式的最朴素落地。