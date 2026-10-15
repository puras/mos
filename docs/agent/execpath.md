# 工具批量执行通道（agent/execpath）

> 来源：`agent/execpath/execpath.go`（架构 v2 §3.9 并发执行器）。
> 目标读者：理解"工具调用是怎么被批量、安全、可观测地执行"的工程师；想自定义工具执行环节（截断/审计/超时）的开发者。

---

## 1. 一句话定位

**`agent/execpath` 是所有驱动（react/scripted 等）共用的"工具批量执行通道"**：把"模型给一组工具调用 → 守卫决策 → 并发执行 → 结果回填"这一完整链路抽出来，让 react 批量和 scripted 编排各波**共用同一套 GuardBus / 审批挂起 / 钩子链 / 截断 / 审计 / tool/call 日志语义**。

源码顶端的注释（execpath.go:1-5）一句话点明价值：

> "react 批量与 scripted 编排各波共用，保证 GuardBus 决策 / 审批挂起 / tool/call 日志 / 钩子链 / 截断 / 审计语义**全库唯一**。语义中立于驱动：不感知 ReAct 或 Scripted 编排，由调用方负责 ApplyOutcomes 落日志收尾"

这就是"语义中立于驱动"的设计：execpath 只管"执行一批工具"，不管"是哪一轮、是不是规划内一步"。

文件只有一个 `execpath.go`，按职责切分为 4 块：

| 块 | 行号 | 职责 |
|---|---|---|
| 类型与错误 | 28-41 | `ErrToolDenied` / `ToolDeniedError` / `ToolOutcome` |
| 批量执行 | 54-114 | `ExecuteBatch`（三阶段核心） |
| 单条执行 | 118-135 | `executeTool`（三段钩子链） |
| 日志收尾 + 默认钩子 | 141-233 | `ApplyOutcomes` / `ExecuteHook` / `TruncateHook` / `AuditHook` |

---

## 2. 三阶段执行模型（架构 v2 §3.9）

源码（execpath.go:43-53）把 `ExecuteBatch` 的语义讲得很清楚：

```
┌─────────────────────────────────────────────────────────────────┐
│ Phase 1: preflight（串行）                                       │
│   - 单调守卫决策（GuardBus.Allow）                                │
│   - 拒绝 = 拦截集合（显式失败回填）                                │
│   - 挂起 = 整批中止（返回第二返回值 *SuspendError）                │
│   - **先于** tool/call 日志 —— 挂起的工具不落日志                  │
├─────────────────────────────────────────────────────────────────┤
│ Phase 2: execute（有界并发，并发度 = rt.MaxToolParallel）          │
│   - pre-execute → execute → post-execute 三段钩子链              │
│   - 任一工具声明 SequentialTool 时该项在批内串行执行               │
├─────────────────────────────────────────────────────────────────┤
│ Phase 3: finalize（调用方负责）                                   │
│   - ApplyOutcomes 按模型调用顺序回填 tool/result + 观测          │
└─────────────────────────────────────────────────────────────────┘
```

**几个值得品的设计点**：

1. **preflight 串行、execute 并发** —— 决策一致性优先（不能让两个并发的 Policy 检查同时见到不一致状态），执行性能次之
2. **挂起信号经闭包捕获 + 取消传播**：批内任一工具触发审批即 `cancel()` 中止后续 preflight / execute（execpath.go:67-68 + 76-77）
3. **挂起时不落 tool/call 日志**：先决策后日志，决策拒绝的工具连日志都不留（execpath.go:85-89）
4. **Doom Loop 致命错误原样返回**：与普通拒绝（ToolDeniedError）分开处理，让循环层能识别并置任务 blocked

---

## 3. `ExecuteBatch` 全实现（execpath.go:54-114）

### 3.1 SequentialTool 标记

```go
if t, ok := rt.Tools.Get(calls[i].Name); ok {
    if st, isSeq := t.(tool.SequentialTool); isSeq && st.Sequential() {
        items[i].Sequential = true
    }
}
```

工具实现可选接口 `tool.SequentialTool`，声明自己**必须在批内串行执行**（不能并发）—— 典型场景：写文件、修改全局状态、不能并发的副作用工具。

### 3.2 闭包捕获挂起信号 + 取消传播

```go
var batchSuspend *approval.SuspendError
bctx, cancel := context.WithCancel(ctx)
defer cancel()
```

**这是精心设计的并发控制**：

- `bctx`：带 cancel 的派生 ctx
- `cancel()` 在两个时机触发：
  1. 函数返回前 `defer cancel()` —— 资源清理
  2. preflight 检测到 `*SuspendError` 时手动 `cancel()` —— 终止后续 preflight/execute goroutine

**关键不变量**：批内任一工具挂起，**整批中止**，未执行的工具不留下半截状态。

### 3.3 preflight 函数（execpath.go:70-91）

```go
preflight := func(ctx context.Context, call *llm.ToolCall) (bool, error) {
    ev := &hook.HookEvent{Kind: hook.HookToolPreExecute, ToolCall: call, Turn: turn, Step: step}
    if err := rt.GuardBus.Allow(ctx, ev); err != nil {
        var s *approval.SuspendError
        if errors.As(err, &s) {
            batchSuspend = s
            cancel()
            return false, err
        }
        var doomErr *hook.DoomLoopError
        if errors.As(err, &doomErr) {
            return false, err  // 致命守卫错误：循环层终止任务
        }
        return false, &ToolDeniedError{Reason: err.Error()}
    }
    // 守卫放行后才落日志 + 观测（挂起/拒绝的工具不产生 tool/call 日志）
    if err := sess.AppendToolCall(turn, call); err != nil {
        return false, err  // 日志写失败：终止任务（非拦截回填）
    }
    rt.Notify(agent.Event{Type: agent.EventToolCall, SessionID: sess.ID, Turn: turn, ToolCall: call})
    return true, nil
}
```

**三个错误分级**：

| 错误类型 | 处理 | 来源 |
|---|---|---|
| `*approval.SuspendError` | 整批中止 + cancel() + 返回 batchSuspend | policyGuard |
| `*hook.DoomLoopError` | 致命，原样返回 → 循环层终止任务（blocked） | DoomLoopGuard |
| 其他（含 `*ToolDeniedError`） | 包装为 `ToolDeniedError` → 进入 Outcome.Denied 集合 | Policy / skill 白名单 / 其他 Guard |

**值得品的两条不变量**：

1. **守卫放行后才落日志**（execpath.go:85）—— 挂起/拒绝的工具连 `tool/call` 都不留，因为它们"没真的发生"
2. **日志写失败非拒绝**（execpath.go:87）—— Append 失败不是"用户拒绝"，是"系统坏了"，必须终止任务

### 3.4 execute 函数（execpath.go:92-111）

```go
execute := func(ctx context.Context, call *llm.ToolCall) (ToolOutcome, error) {
    ctx = tool.WithCallID(ctx, call.ID)
    if rt.Approval != nil {
        ctx = approval.WithApproval(ctx, rt.Approval)
    }
    if rt.ResumeCheckpoint != nil {
        ctx = approval.WithResumeDecisions(ctx, rt.ResumeCheckpoint.Decisions)
    }
    var out ToolOutcome
    err := telemetry.WithSpan(ctx, rt.Telemetry, telemetry.SpanToolExec,
        map[string]string{"tool": call.Name, "call_id": call.ID, "turn": strconv.Itoa(turn)},
        func(ctx context.Context) error {
            var err error
            out, err = executeTool(ctx, rt, call, turn, step)
            return err
        })
    return out, err
}
```

**注入 ctx 的三样东西**：

| 注入项 | 用途 |
|---|---|
| `tool.WithCallID` | 工具能拿到自己的 CallID（如要在结果里回传） |
| `approval.WithApproval` | request_human_input 等工具能从 ctx 拿到审批通道 |
| `approval.WithResumeDecisions` | Resume 路径让工具能读到人类回答 |

然后用 `telemetry.WithSpan` 包一层开 span，自动打 tool/turn/call_id 属性 —— **可观测性是默认行为**，不需要业务额外接。

### 3.5 返回值

```go
return outs, batchSuspend
```

两个返回值含义：

| 返回 | 含义 |
|---|---|
| `[]Outcome` | 即使有挂起/拒绝也有值（部分项 Denied） |
| `*SuspendError` | 非 nil 表示整批已中止，**调用方必须先处理 suspend 再 ApplyOutcomes** |

注释（execpath.go:53）：

> "已通过守卫并落日志的先行工具可能留下孤儿 tool/call，不影响模型面投影"

也就是说：**挂起前可能已有部分工具执行完成**（preflight 是串行但 execute 是并发），它们的 tool/call 日志已落，但 Outcome.Denied 集合的逻辑是"preflight 拒绝"，所以**正常完成执行的工具**不在 Denied 集合里，调用方正常回填；**只在 preflight 阶段被挂起/拒绝的**才进 Denied。

---

## 4. `ToolOutcome`：单条工具执行结果

```go
type ToolOutcome struct {
    Result any     // 原始返回值（审计/平台观测用）
    Text   string  // 给模型看的文本（截断后）
    Err    error   // 运行期错误
}
```

与 `event.ToolResultData` 字段对应（详见 event.md §3.3）：

| Outcome | 对应 ToolResultData | 说明 |
|---|---|---|
| `Result` | `Result` | 原始结构化，永久保留 |
| `Text` | `Text` | 截断后的模型可见文本 |
| `Err` | `Error` | 工具执行错误 |

**Result 与 Text 分离** —— 审计能查原始数据，模型只看到精简文本（节省 token），与 event.md 的设计一致。

---

## 5. `executeTool`：单条工具的三段钩子链（execpath.go:118-135）

```go
func executeTool(ctx context.Context, rt *driver.RunContext, call *llm.ToolCall, turn, step int) (ToolOutcome, error) {
    ev := &hook.HookEvent{Kind: hook.HookToolPreExecute, ToolCall: call, Turn: turn, Step: step}
    ev, err := rt.HookBus.Waterfall(ctx, ev)  // pre-execute（可改写/拒绝）
    if err != nil { return ToolOutcome{Err: err}, nil }
    
    execEv := &hook.HookEvent{Kind: hook.HookToolExecute, ToolCall: ev.ToolCall, Turn: turn, Step: step}
    execEv, err = rt.HookBus.Waterfall(ctx, execEv)
    if err != nil { return ToolOutcome{Err: err}, nil }
    
    postEv := &hook.HookEvent{Kind: hook.HookToolPostExecute, ToolCall: ev.ToolCall, 
                                Result: execEv.Result, Err: execEv.Err, Turn: turn, Step: step}
    postEv, err = rt.HookBus.Waterfall(ctx, postEv)
    if err != nil { return ToolOutcome{Err: err}, nil }
    
    return ToolOutcome{Result: execEv.Result, Text: postEv.ResultText, Err: postEv.Err}, nil
}
```

### 5.1 三段对应三个 HookKind

```
HookToolPreExecute  → HookToolExecute → HookToolPostExecute
       ↓                    ↓                    ↓
   改写/拒绝          around dispatch         截断/审计
   (Policy/审批)    (执行/超时/重试)        (TruncateHook/AuditHook)
```

**关键不变量**：

1. **`ev.ToolCall` 跨阶段传递** —— pre-execute 可改写 call.Name/Args，下一阶段拿到的是改写后的（execpath.go:124）
2. **`postEv` 同时拿到 Result 和 Err** —— 失败也能进 post-execute 做审计（详见 §8 AuditHook）
3. **任一阶段 err 立即返回** —— 不继续走后续阶段

### 5.2 错过的工具返回值流向

| 阶段 | 写回字段 |
|---|---|
| pre-execute 拒绝 | （不进 execute/post） |
| execute 错误 | `execEv.Err`，传到 post |
| post 改写 | `postEv.ResultText`，写到 `ToolOutcome.Text` |
| 全成功 | `ToolOutcome{Result: execEv.Result, Text: postEv.ResultText, Err: nil}` |

---

## 6. `ApplyOutcomes`：日志收尾（execpath.go:141-157）

```go
func ApplyOutcomes(ctx, rt, sess, turn, calls, outs) error {
    for _, out := range outs {
        call := out.Input
        if out.Denied {
            var deniedErr *ToolDeniedError
            if errors.As(out.Err, &deniedErr) {
                _ = sess.AppendToolResult(turn, call, nil, "", deniedErr)
                rt.Notify(agent.Event{Type: agent.EventToolResult, ...})
                continue
            }
            return fmt.Errorf("agent: tool preflight: %w", out.Err)
        }
        _ = sess.AppendToolResult(turn, call, out.Output.Result, out.Output.Text, out.Output.Err)
        rt.Notify(...)
    }
    return nil
}
```

### 6.1 Denied 项处理

| out.Err 类型 | 处理 |
|---|---|
| `*ToolDeniedError` | 落 tool/result（带拒绝原因） + 观测，继续 |
| 其他 | 返回 error 终止任务（preflight 阶段的非守卫错误，如日志写失败） |

注释（execpath.go:139-140）：

> "挂起（*SuspendError）与致命（*hook.DoomLoopError）由调用方在调用本函数**前**检测处理"

也就是说：调用方（react 的 `finalizeToolBatch`）必须先扫 Outcome 找 SuspendError 写 checkpoint，扫 DoomLoopError 终止任务，**剩下才是 ApplyOutcomes 的工作**。

### 6.2 顺序保证

注释（execpath.go:48）：

> "结果按模型调用顺序回填（本函数只产出按源序的 Outcome）"

调用方传入的 `calls` 顺序 = 模型输出顺序 = 模型期望看到 tool/result 的顺序。`executor.Run` 保证 Outcome 顺序与输入顺序一致，所以这里 `for _, out := range outs` 就是按模型序回填。

---

## 7. `ExecuteHook`：ctx 注入点（execpath.go:170-194）

这是所有工具调用前的"ctx 工厂"，源码顶部的注释（execpath.go:162-170）列得很全：

```go
func ExecuteHook(rt *driver.RunContext) hook.Hook {
    return func(ctx context.Context, ev *hook.HookEvent, next hook.Next) (*hook.HookEvent, error) {
        if ev.ToolCall == nil { return next(ctx, ev) }
        
        // 注入四样能力
        if rt != nil && rt.Session != nil {
            ctx = agent.WithSession(ctx, rt.Session)
        }
        if rt != nil && rt.FS != nil {
            ctx = fs.WithFileSystem(ctx, rt.FS)
        }
        if rt != nil && rt.EventBus != nil {
            ctx = fs.WithBus(ctx, rt.EventBus)
        }
        if rt != nil && rt.HTTPClient != nil {
            ctx = httpx.WithClient(ctx, rt.HTTPClient)
        }
        
        result, err := rt.Tools.Call(ctx, ev.ToolCall.Name, json.RawMessage(ev.ToolCall.Args))
        ev.Result = result
        if err != nil { ev.Err = err }
        return next(ctx, ev)
    }
}
```

### 7.1 注入的四样能力

| 注入 | 工具 | 读取方式 | nil 兜底 |
|---|---|---|---|
| `*session.Session` | activate_skill 等需要写事件 / 读本轮状态 | `agent.SessionFrom(ctx)` | 工具要自己处理 |
| `rt.FS` | 内置文件工具 read_file/write_file/glob/grep | `fs.FromContext(ctx)` | **fail-closed**（`*fs.NotConfiguredError`） |
| `rt.EventBus` | Sandboxed.Resolve 审计事件出口 | `fs.BusFrom(ctx)` | 不注入则不发 |
| `rt.HTTPClient` | 联网工具 web_fetch/web_search | `httpx.FromContext(ctx)` | `httpx.Default`（带超时） |

注释（execpath.go:166-167）说明 FS 的设计：

> "未注入即 *fs.NotConfiguredError（fail-closed）。**注入点集中在此（三个驱动共用 execpath 通道），一处覆盖全部路径**"

这是**统一入口**设计：FS 不是驱动各自注入，是集中在 ExecuteHook 一处，三个驱动共用 execpath 时**一处覆盖全部路径**。

### 7.2 Hook 链中的位置

```
tool/pre-execute  →  tool/execute  →  tool/post-execute
   (Policy)        (ExecuteHook)        (Truncate/Audit)
                   ↑
                   链尾默认执行器
```

ExecuteHook 是 tool/execute 链尾的"默认执行器"（execpath.go:159-160）：

> "平台可在其前注册包装钩子（超时/重试/指标），经 next 委托到本执行器"

也就是说：**平台想加超时/重试/指标**，注册到 `hook.HookToolExecute` 即可，自动包在 ExecuteHook 外面：

```
平台超时钩子
  ↓ next
ExecuteHook（默认执行器，调 Tools.Call）
  ↓ next
（链尾）
```

---

## 8. `TruncateHook` 与 `AuditHook`（execpath.go:198-233）

两个 post-execute 钩子，对应 system.md 提到的"模型可见文本瘦身"和 session.md 提到的"已验证事实写入"。

### 8.1 TruncateHook（execpath.go:198-211）

```go
func TruncateHook(rt *driver.RunContext) hook.Hook {
    return func(ctx, ev, next) {
        if ev.ToolCall == nil || rt.Truncator == nil { return next(ctx, ev) }
        text := session.FormatToolResult(ev.Result)
        if s, truncated := rt.Truncator.Truncate(ev.ToolCall.Name, text); truncated {
            ev.ResultText = s
        } else {
            ev.ResultText = text
        }
        return next(ctx, ev)
    }
}
```

**只改 `ev.ResultText`（模型面），不改 `ev.Result`（审计面）** —— 这就是"审计保真 + 模型瘦身"双轨的实现。

### 8.2 AuditHook（execpath.go:215-233）

```go
func AuditHook(rt *driver.RunContext) hook.Hook {
    return func(ctx, ev, next) {
        if ev.ToolCall == nil || ev.Err != nil || rt.Auditor == nil { return next(ctx, ev) }
        facts, err := rt.Auditor.Audit(ctx, ev.ToolCall, ev.Result)
        if err != nil { return next(ctx, ev) }
        
        // 批量工具并发执行时多个 goroutine 同时写同一 Session.State.Facts，
        // 经 rt.AddFact 串行化（锁外广播 Fact 事件，避免 Observer 回调持锁）。
        for _, f := range facts {
            rt.AddFact(f)
            f := f
            rt.Notify(agent.Event{Type: agent.EventFact, ...})
        }
        return next(ctx, ev)
    }
}
```

**几个值得品的细节**：

1. **`ev.Err != nil` 跳过审计** —— 失败的工具结果不写入 Fact（避免污染可信状态）
2. **`f := f` 循环变量捕获** —— Go 1.22 之前需要这行显式复制循环变量给闭包（避免所有 goroutine 共享同一个 f）
3. **`rt.AddFact` 串行化 + `rt.Notify` 锁外** —— 注释（execpath.go:225-226）解释：
   > "经 rt.AddFact 串行化（锁外广播 Fact 事件，避免 Observer 回调持锁）"

   即：先把 Fact 加到 Session.State（持锁），再释放锁去调 Observer（不持锁）。如果 Observer 回调里又调 AddFact，不会死锁。

### 8.3 post-execute 钩子顺序

```
TruncateHook → AuditHook
```

注释（react.go:65-67）说明默认注册顺序：

```go
rt.HookBus.On(hook.HookToolPostExecute, execpath.TruncateHook(rt))
rt.HookBus.On(hook.HookToolPostExecute, execpath.AuditHook(rt))
```

**先截断后审计**是有讲究的：审计看的是 `ev.Result`（原始 Result），截断只影响 `ev.ResultText`（模型面）—— 两个钩子各管一摊，互不干扰，顺序无关紧要但**显式有序**更易理解。

---

## 9. 与 executor 包的关系

`ExecuteBatch` 用 `executor.Run` 做底层并发执行：

```go
outs := executor.Run(bctx, items, rt.MaxToolParallel, preflight, execute)
```

`executor` 包提供：
- `Item[Input]{Input, Sequential}`：执行单元
- `Outcome[Input, Output]{Input, Output, Denied, Err}`：执行结果（含 Denied 标记）
- `Run(ctx, items, parallelism, preflight, execute)`：并发执行

**execpath 是策略，executor 是机制**。execpath 把"preflight 拒绝/挂起/DoomLoop 三种错误"、"ctx 注入"、"钩子链"这些语义打包好，丢给 executor 这个通用并发器执行。

---

## 10. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **preflight 串行、execute 并发** | ExecuteBatch 三阶段 | 决策一致性 + 执行性能 |
| **挂起时整批中止** | cancel() + batchSuspend 闭包捕获 | 不留半截状态 |
| **守卫放行后才落 tool/call 日志** | preflight 第 85 行 | 拒绝的工具不留痕 |
| **日志写失败非拒绝** | preflight 第 87 行 | 系统错误要终止任务 |
| **DoomLoop 致命 vs ToolDenied 软拒绝** | 错误分级 | 不同下游处理 |
| **Result 与 Text 双轨** | ToolOutcome 两字段 | 审计保真 + 模型瘦身 |
| **fs capability 注入集中一处** | ExecuteHook | 一处覆盖全路径 |
| **fs 缺则 fail-closed** | fs.NotConfiguredError | 文件操作必须配沙箱 |
| **HTTP nil 自动兜底** | httpx.Default | 联网工具免配 |
| **审计失败的 Result 不写入 Fact** | AuditHook ev.Err != nil 跳过 | 失败事实不算事实 |
| **AddFact 串行 + Notify 锁外** | rt.AddFact 注释 | 防 Observer 回调死锁 |

---

## 11. 自定义扩展姿势

### 11.1 加工具超时钩子

```go
rt.HookBus.On(hook.HookToolExecute, func(ctx, ev, next) {
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()
    return next(ctx, ev)
})
```

注册到 tool/execute，自动包在 ExecuteHook 外面。

### 11.2 加自定义 Truncator

```go
rt.Truncator = myTruncator{}  // 实现 agent.ResultTruncator
// 替换默认 NewDefaultTruncator
```

### 11.3 加自定义 Auditor

```go
rt.Auditor = myAuditor{}  // 实现 agent.Auditor
// 替换默认 NewDefaultAuditor
```

### 11.4 工具声明 SequentialTool

```go
type MyWriteFile struct{}

func (t *MyWriteFile) Sequential() bool { return true }  // 强制批内串行

func (t *MyWriteFile) Call(ctx context.Context, args json.RawMessage) (any, error) { ... }
```

执行器会自动识别，按 items[i].Sequential=true 串行调度。

### 11.5 自定义 pre-execute 钩子改写参数

```go
rt.HookBus.On(hook.HookToolPreExecute, func(ctx, ev, next) {
    if ev.ToolCall.Name == "shell" {
        // 给所有 shell 命令注入 wrapper
        ev.ToolCall.Args = wrapArgs(ev.ToolCall.Args)
    }
    return next(ctx, ev)
})
```

`ev.ToolCall` 改写会传到 execute 阶段。

---

## 12. 一句话总结

**`agent/execpath` 是 react/scripted 共用的"工具批量执行通道"：用三阶段模型（preflight 串行守卫 → execute 有界并发 → finalize 顺序回填）把"决策一致性 + 执行性能 + 日志保真"三个目标同时实现，用 `ExecuteBatch` + `ApplyOutcomes` 的两步 API 让调用方只需关心"启动 + 收尾"，用 `ExecuteHook` 集中注入 ctx 能力（FS/HTTP/Session/EventBus）让一处覆盖全路径，用 `TruncateHook` + `AuditHook` 两个默认 post-execute 钩子把"模型瘦身 + 审计保真"双轨落地。**它是 MoS 工具执行层的"语义中立内核" —— 任何驱动想批量执行工具都应该走这条通道，复用同一套 Guard/审批/钩子/审计语义。