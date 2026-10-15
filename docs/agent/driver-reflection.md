# ReflectionDriver —— 生成-反思-修订循环驱动

> 来源：`agent/driver/reflection/reflection.go`（架构 v2 §3.3，Agentic Design Patterns 自查模式）。
> 目标读者：理解"先产出再自查"循环的所有人；想自定义评审器（独立上下文避免自我合理化）的开发者。

---

## 1. 一句话定位

**`ReflectionDriver` 是 MoS 的"生成 → 反思 → 修订"自查循环驱动**：调内嵌 ReActDriver 出初稿，再用**独立上下文的评审器**（`planner.Reflector`，避免"自我合理化"污染）打分提意见，**带意见让内嵌执行器修订**，**直至评审通过或达到最大迭代数**。

源码顶端的注释（reflection.go:1-5）一段话讲清定位：

> "Package reflection 提供 Basic Reflection 循环驱动（架构 v2 §3.3，Agentic Design Patterns 自查模式）：**Generate → Reflect → Revise 直至评审通过或达到最大迭代**。与 ReActDriver 的关系：Generate/Revise 复用内嵌 ReAct 驱动（支持工具），**Reflect 使用独立评审器（planner.Reflector，独立上下文避免"自我合理化"污染）**。"

文件只有一个 `reflection.go`（167 行），加测试 1 个文件。

---

## 2. 数据结构 + 三个 Option

```go
const defaultMaxIterations = 2

type ReflectionDriver struct {
    generator     driver.LoopDriver  // 初稿生成/修订执行器（默认 ReActDriver）
    reflector     planner.Reflector  // 评审器（独立上下文）
    maxIterations int                // 最大修订轮数（>0）
}

type Option func(*ReflectionDriver)
```

| Option | 默认 | 用途 |
|---|---|---|
| `WithGenerator(driver.LoopDriver)` | `&react.ReActDriver{}` + `react.NewReAct(rt)` | 替换生成/修订执行器 |
| `WithReflector(planner.Reflector)` | `rt.Reflector` | 注入评审器（**均未设置时拒绝运行**） |
| `WithMaxIterations(int)` | `2` | 设置最大修订轮数（>0 才生效） |

**`WithMaxIterations` 注释**（reflection.go:47-52）：

```go
func WithMaxIterations(n int) Option {
    return func(r *ReflectionDriver) {
        if n > 0 {
            r.maxIterations = n   // n <= 0 保持默认
        }
    }
}
```

—— `n <= 0` 时**保持默认值**而不是覆盖为 0（防呆）。

---

## 3. `New` 装配（reflection.go:57-70）

```go
func New(rt *driver.RunContext, opts ...Option) *driver.Agent {
    d := &ReflectionDriver{maxIterations: defaultMaxIterations}
    for _, o := range opts { o(d) }
    if d.generator == nil {
        d.generator = &react.ReActDriver{}
        react.NewReAct(rt)   // ← 关键：补全 rt 默认钩子
    }
    if d.reflector == nil {
        d.reflector = rt.Reflector
    }
    return driver.New(rt, d)
}
```

**关键设计点**（注释 reflection.go:54-56）：

> "默认钩子/守卫装配与 react.New 一致（同一 rt 只应由一个驱动入口装配，避免默认钩子重复注册）"

—— 与 PlanExecuteDriver 同样的"避免重复注册"约束。

---

## 4. `Run` 主循环（reflection.go:73-137）

### 4.1 五段式流程

```
┌─ 初始化
│   ├─ 检查 rt.Session / rt.Tools
│   ├─ 裸实例惰性装配（generator / reflector 为 nil 时补 ReActDriver / rt.Reflector）
│   ├─ reflector == nil 二次检查 → 拒绝运行（fail-closed）
│   ├─ sandbox.WithRunner 注入 runner
│   └─ TaskState.Goal / Status 初始化
├─ turn = sess.NextTurn()  // 注意：内嵌执行器还会开 turn
├─ Generate（初稿）
│   ├─ 写 reflection/start 事件
│   └─ draft, err := d.generator.Run(ctx, rt, input)
├─ Reflect → Revise 循环（iter=1..maxIterations）
│   ├─ review, err := d.reflector.Reflect(ctx, input, draft, llmMsgs)
│   ├─ 写 reflection/review 事件 + 广播 EventReflection
│   ├─ review.Issues 为空 → finish（completed）
│   └─ 否则：
│       ├─ 写 user/message(reviewText)（"Model-visible means logged"）
│       └─ draft, err = d.generator.Run(ctx, rt, "请根据上面的评审意见修订...")
├─ 达到最大迭代仍未通过 → finish（max_iterations）
└─ finish：写 reflection/end + EventDone
```

### 4.2 裸实例惰性装配 + 强制 reflector（reflection.go:80-90）

```go
if d.generator == nil { d.generator = &react.ReActDriver{} }
if d.reflector == nil { d.reflector = rt.Reflector }
if d.reflector == nil {
    return "", fmt.Errorf("agent: reflection requires a reflector (rt.Reflector or WithReflector)")
}
```

**关键设计点**：

| 设计 | 含义 |
|---|---|
| **裸实例惰性装配** | Router 路径补 generator |
| **两次 reflector == nil 检查** | New 没设 + rt.Reflector 也没设 → **fail-closed 拒绝**（reflection 没有"单步退化"模式，没评审器就跑不动） |
| **错误信息明确** | "reflection requires a reflector (rt.Reflector or WithReflector)" 列出注入位 |

### 4.3 turn 由谁开？（reflection.go:103）

```go
// 任务回合由内嵌执行器（Generate 阶段）开启：ReAct 会写 turn/start +
// user/message(input)，此处不再重复；reflection 事件统一以起始 Turn 标记。
turn := sess.NextTurn()
```

注释讲清 turn 开者：

> "**任务回合由内嵌执行器（Generate 阶段）开启**：ReAct 会写 turn/start + user/message(input)，此处不再重复；reflection 事件统一以起始 Turn 标记。"

—— **与 planexecute 相反**：planexecute 由外层开 turn，reflection 由内嵌开。**reflection 只取一次 turn 作为 reflection 事件关联键，不重复开启**。

### 4.4 Generate → Reflect → Revise 三段（reflection.go:106-135）

```go
// Generate：内嵌执行器产出初稿
_ = sess.Append(event.SessionEventReflectionStart, event.ReflectionStartData{
    Turn: turn, Goal: input, MaxIterations: d.maxIterations,
})
draft, err := d.generator.Run(ctx, rt, input)
if err != nil { return d.fail(sess, turn, 0, err) }

// Reflect → Revise 循环
for iter := 1; iter <= d.maxIterations; iter++ {
    review, err := d.reflector.Reflect(ctx, input, draft, session.ConvertToLlm(sess.Messages()))
    if err != nil { return d.fail(sess, turn, iter, err) }
    
    _ = sess.Append(event.SessionEventReflectionReview, event.ReflectionReviewData{
        Turn: turn, Iteration: iter, Issues: review.Issues, Score: review.Score, Passed: len(review.Issues) == 0,
    })
    rt.Notify(agent.Event{Type: agent.EventReflection, SessionID: sess.ID, Turn: turn, Content: reviewText(iter, review)})
    
    if len(review.Issues) == 0 {
        return d.finish(sess, rt, turn, iter, "completed", draft)
    }
    
    // Revise：评审意见写入日志（Model-visible means logged），再让执行器修订
    _ = sess.AppendUserMessage(reviewText(iter, review), "reflection")
    draft, err = d.generator.Run(ctx, rt, "请根据上面的评审意见修订产出，直接输出修订后的完整内容。")
    if err != nil { return d.fail(sess, turn, iter, err) }
}
return d.finish(sess, rt, turn, d.maxIterations, "max_iterations", draft)
```

**四个关键设计点**：

| 设计 | 含义 |
|---|---|---|
| **`review.Issues == 0` 即视为通过** | 评审器**自己决定标准**（不用 Score 阈值，简化语义） |
| **`reviewText` 写 user/message(reviewText, "reflection")`** | "Model-visible means logged"（注释 reflection.go:129）：让模型能看到意见 + 日志保留 |
| **修订指令 hardcode** | "请根据上面的评审意见修订产出，直接输出修订后的完整内容。" 让执行器知道要做什么 |
| **`fail(turn, iter, err)`** | iter=0 是 Generate 失败，1..N 是第 iter 次 Revise 失败 |

### 4.5 `finish` + `fail` 收尾（reflection.go:140-152）

```go
func (d *ReflectionDriver) finish(sess, rt, turn, iter, status, draft) (string, error) {
    _ = sess.Append(event.SessionEventReflectionEnd, event.ReflectionEndData{Turn: turn, Iteration: iter, Status: status})
    sess.State.Status = session.StatusCompleted
    rt.Notify(agent.Event{Type: agent.EventDone, SessionID: sess.ID, Turn: turn, Content: draft})
    return draft, nil
}

func (d *ReflectionDriver) fail(sess, turn, iter, err) (string, error) {
    _ = sess.Append(event.SessionEventReflectionEnd, event.ReflectionEndData{Turn: turn, Iteration: iter, Status: "failed", Detail: err.Error()})
    sess.State.Status = session.StatusBlocked
    return "", fmt.Errorf("agent: reflection: %w", err)
}
```

**两个收尾分支**：

| 状态 | 触发 | TaskState | 事件 |
|---|---|---|---|
| `completed` | 通过（无 Issues） | StatusCompleted | reflection/end + status="completed" |
| `max_iterations` | 达到上限仍有 Issues | StatusCompleted | reflection/end + status="max_iterations" |
| `failed` | err | StatusBlocked | reflection/end + status="failed" + Detail |

**注意**：`max_iterations` 也算 **StatusCompleted**（任务完成但未达完美），区别于 `failed` 的 Blocked。

---

## 5. `reviewText` 评审文本渲染（reflection.go:155-166）

```go
func reviewText(iter int, review planner.Review) string {
    var b strings.Builder
    fmt.Fprintf(&b, "评审意见（第 %d 轮）", iter)
    if review.Score >= 0 {
        fmt.Fprintf(&b, "，质量分 %d", review.Score)
    }
    b.WriteString("：\n")
    for i, issue := range review.Issues {
        fmt.Fprintf(&b, "%d. %s\n", i+1, issue)
    }
    return strings.TrimSpace(b.String())
}
```

**两个不变量**：

| 不变量 | 含义 |
|---|---|
| **`Score < 0` 不显示分** | `planner.Review.Score = -1` 表示未打分 |
| **`Issues` 1-based 序号** | 评审文本易读 |

**渲染示例**：

```
评审意见（第 2 轮），质量分 75：
1. 第 3 段逻辑不清
2. 缺少具体示例
3. 术语翻译不准确
```

—— 这段文本**既**作为 EventReflection.Content 通知前端，**又**作为 AppendUserMessage(reviewText) 进入下次修订的上下文。

---

## 6. 事件链

```
// Generate（第一次 generator.Run）
//   └─ 内嵌 ReActDriver 自动写：turn/start, user/message(input), llm/request, ... turn/end

SessionEventReflectionStart{Turn, Goal, MaxIterations}
   // 循环 iter 1..maxIterations
   SessionEventReflectionReview{Turn, Iteration, Issues, Score, Passed}
      └─ EventReflection 通知
   (有 Issues 时)
   SessionEventUserMessage(reviewText, "reflection")  // "Model-visible means logged"
      └─ 内嵌 ReActDriver 第二次：turn/start, llm/request, ... turn/end

SessionEventReflectionEnd{Turn, Iteration, Status: "completed"|"max_iterations"|"failed"}
   └─ EventDone 通知
```

**事件关键字段**：

| 字段 | 含义 |
|---|---|
| `ReflectionStart.MaxIterations` | 配置的最大轮数 |
| `ReflectionReview.Iteration` | 第几轮（1-based） |
| `ReflectionReview.Issues` | 评审器返回的问题列表 |
| `ReflectionReview.Score` | 质量分（-1 = 未打分） |
| `ReflectionReview.Passed` | Issues 是否为空 |
| `ReflectionEnd.Iteration` | 最终轮次 |
| `ReflectionEnd.Status` | completed / max_iterations / failed |
| `ReflectionEnd.Detail` | failed 时记 err |

---

## 7. 与 ReActDriver / PlanExecuteDriver 的关系

| 维度 | ReActDriver | PlanExecuteDriver | ReflectionDriver |
|---|---|---|---|
| 入口 | `Driver="react"` | `Driver="plan-execute"` | `Driver="reflection"` |
| 任务循环 | 单轮循环 | 写计划 + 多步循环 | 生成 + 反思 + 修订循环 |
| 步骤执行 | 自身 | 内嵌 ReActDriver | **内嵌 ReActDriver（Generate/Revise）** |
| 评审器 | — | — | **独立 reflector**（避免自我合理化） |
| 校验器 | — | `WithVerifier(VerifyFunc)` | — |
| 共享 RT | ✅ | ✅ | ✅ |
| 同 Turn | 单 Turn 单步 | 单 Turn 多步 | 单 Turn 多迭代 |
| 必需组件 | — | PlanPlanner（可选） | **Reflector（必需）** |

**关键差异**：reflection 强制要求 `Reflector`（无 fallback）—— 因为 reflection 没有"退化模式"，没评审器就跑不动。

---

## 8. 为什么评审器要独立上下文？

注释（reflection.go:1-5）：

> "**Reflect 使用独立评审器（planner.Reflector，独立上下文避免"自我合理化"污染）**"

**问题**：如果让同一模型既当"作者"又当"评审员"，模型会**倾向于赞同自己的输出**（自我合理化 bias）—— 评审形同虚设。

**MoS 解法**：

- **作者**：内嵌 ReActDriver（主模型，有工具，可调用函数）
- **评审员**：`planner.Reflector` 接口，**独立实现**
  - 可以是**不同模型**（如主模型用 GPT-4，评审用 Claude）
  - 可以是**不同 prompt**（专门 critic prompt）
  - 可以是**程序化校验**（planner.Reflector 接口实现可以做任意检查）
- **会话历史 vs 评审上下文**：`Reflect(ctx, input, draft, msgs)` 把 `draft` + `msgs` 显式传给评审器，**评审器自己决定用什么上下文**

—— 这是 v2 §3.3 Reflection 模式的关键设计：评审员**不能是同模型同上下文**。

---

## 9. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **reflector == nil 拒绝运行** | Run 二次检查 | fail-closed |
| **`maxIterations <= 0` 保持默认** | WithMaxIterations | 防呆 |
| **同 rt 默认钩子只装一次** | New 注释 + `react.NewReAct(rt)` | 避免重复 |
| **裸实例惰性装配** | Run 顶部 `generator == nil` 检查 | router 兜底 |
| **评审意见写日志** | AppendUserMessage(reviewText, "reflection") | Model-visible means logged |
| **`Issues == 0` 即通过** | for 循环 break | 评审器自决标准 |
| **同 Turn 多迭代** | turn 不递增 | 任务级粒度 |
| **`max_iterations` 也算 completed** | finish 分支 | 不阻塞任务完成 |
| **评审器独立上下文** | Reflector 接口 | 避免自我合理化 |
| **`fail` 写 detail** | SessionEventReflectionEnd.Detail | 审计可读 |

---

## 10. 自定义扩展姿势

### 10.1 注入自定义评审器（不同模型评审）

```go
type CriticReflector struct {
    critic llm.ChatModel  // 独立模型（如 Claude）
}

func (r *CriticReflector) Reflect(ctx context.Context, input, draft string, msgs []llm.Message) (planner.Review, error) {
    resp, err := r.critic.Chat(ctx, &llm.Request{
        Messages: []llm.Message{
            {Role: llm.RoleSystem, Content: "你是一位严苛的代码评审员。指出初稿的具体问题并打分（0-100）。"},
            {Role: llm.RoleUser, Content: fmt.Sprintf("任务：%s\n\n初稿：\n%s\n\n请评审。", input, draft)},
        },
    })
    if err != nil { return planner.Review{}, err }
    return parseReview(resp), nil  // 解析 Issues + Score
}

agent, _ := assistant.New(model, tools, assistant.Config{
    Driver: "reflection",
    Reflector: &CriticReflector{critic: claudeModel},
    FileAccess: ...,
})
```

### 10.2 注入程序化校验（不调模型）

```go
type CodeStyleReflector struct{}

func (r *CodeStyleReflector) Reflect(ctx context.Context, input, draft string, msgs []llm.Message) (planner.Review, error) {
    var issues []string
    if !strings.Contains(draft, "func ") { issues = append(issues, "missing func keyword") }
    if len(draft) > 10000 { issues = append(issues, "code too long") }
    if !goLintOK(draft) { issues = append(issues, "go lint failed") }
    return planner.Review{Issues: issues, Score: 100 - len(issues)*10}, nil
}

agent, _ := assistant.New(model, tools, assistant.Config{
    Driver: "reflection",
    Reflector: &CodeStyleReflector{},
})
```

### 10.3 自定义最大迭代数

```go
agent, _ := assistant.New(model, tools, assistant.Config{
    Driver: "reflection",
    Reflector: myCritic,
    DriverFactory: func(rt *driver.RunContext) *driver.Agent {
        return reflection.New(rt, reflection.WithMaxIterations(5))
    },
})
```

### 10.4 嵌套：每轮迭代用 plan-execute

```go
// 每轮 Generate/Revise 都是 plan-execute（递归）
inner := planexecute.New(rt)

outer := reflection.New(rt,
    reflection.WithGenerator(inner),
    reflection.WithReflector(myCritic),
)
```

---

## 11. 一句话总结

**`ReflectionDriver` 是 MoS 的"生成-反思-修订"自查循环驱动：内嵌 ReActDriver 出初稿（Generate）→ 独立评审器 Reflector 提意见（Reflect，独立上下文避免自我合理化）→ 评审意见写日志 + 内嵌执行器修订（Revise）→ 直至 Issues 为空（completed）或达到 maxIterations（max_iterations，仍算 completed）。** 与 PlanExecuteDriver 共享"内嵌 ReAct + 同 rt 钩子"模式，但**强制要求 Reflector**（无 fallback，fail-closed 拒绝运行），评审意见"Model-visible means logged"既通知前端又注入下次上下文 —— 是 v2 §3.3 Agentic Design Patterns 自查模式的最朴素落地。