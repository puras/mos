# ReAct 循环驱动（agent/driver/react）

> 来源：`agent/driver/react/react.go`（架构 v2 §3.3 / §3.4 / §3.9 / §3.11）。
> 目标读者：理解"默认 ReAct 循环是怎么跑的"的所有人；想自定义钩子/守卫接入点的开发者。

---

## 1. 一句话定位

**`agent/driver/react` 是 MoS 的默认驱动**：实现 `LoopDriver.Run`（架构 v2 §3.3 的四种范式之一），负责"推理 → 工具调用 → 直至直接作答"的完整主循环；同时也是默认钩子/守卫的**装配入口**（`InstallDefaults` / `NewReAct` / `New`）。

文件只有一个 `react.go`，但内部按职责切分为 4 块：

| 块 | 行号范围 | 职责 |
|---|---|---|
| 装配入口 | 1-94 | `InstallDefaults` / `NewReAct` / `New` |
| 主循环 | 97-164 | `Run` |
| 单轮循环 | 169-339 | `runTurn` / `runTurnInner` / `runResumeTurn` / `finalizeToolBatch` |
| 内部实现 | 343-665 | 规划调用、流式、checkpoint 落库、钩子/守卫实现 |

---

## 2. 三个装配入口的语义层次

源码（react.go:38-94）给出了**层层叠加**的三个函数：

```go
InstallDefaults(rt)     // 只注册默认钩子/守卫，不注册 Doom Loop
NewReAct(rt)            // = InstallDefaults + Doom Loop + 返回驱动实例
New(rt)                 // = NewReAct + 包成 Agent，业务只用这个
```

注释（react.go:38）解释为什么这么分：

> "react 与 scripted 复用：pre-step compressHook + ragInjectHook，tool/execute execpath.ExecuteHook，tool/post-execute execpath.TruncateHook + execpath.AuditHook，Guards policyGuard。**不注册 DoomLoopGuard（仅 react.NewReAct 才有，脚本中重复同工具调用是合法语义）**。"

也就是说：

| 函数 | DoomLoopGuard | 用在哪儿 |
|---|---|---|
| `InstallDefaults` | ❌ | scripted 复用（脚本里重复同工具合法） |
| `NewReAct` | ✅ | 默认入口 |
| `New` | ✅ | 业务方直接用 |

---

## 3. 默认钩子/守卫全清单

`InstallDefaults`（react.go:46-76）注册的全套：

```go
// Hooks
rt.HookBus.On(hook.HookPreStep, compressHook(rt))              // fresh-context 压缩
rt.HookBus.On(hook.HookPreStep, ragInjectHook(rt))             // RAG 检索注入
rt.HookBus.On(hook.HookToolExecute, execpath.ExecuteHook(rt))  // 工具执行（execpath 共享）
rt.HookBus.On(hook.HookToolPostExecute, execpath.TruncateHook(rt)) // 结果截断
rt.HookBus.On(hook.HookToolPostExecute, execpath.AuditHook(rt))    // 审计门（写 Fact）

// Guards
if rt.Policy != nil {
    rt.GuardBus.Add(policyGuard(rt))                            // Policy 单调守卫 + 审批
}
rt.GuardBus.Add(skillAllowListGuard(rt))                        // 技能白名单
```

`NewReAct` 额外加一条：

```go
rt.GuardBus.Add(hook.NewDoomLoopGuard(rt.DoomLoopMaxRepeat).Guard) // Doom Loop（仅 ReAct）
```

### 3.1 装配规则

注释（react.go:71-74）解释 `skillAllowListGuard` 的位置：

> "技能允许工具白名单：激活 Skill 时通过 agent.AddTurnSkill 把 allowed-tools 写入 sess.TurnToolSubset（与 policyGuard 同样在 preflight 串行裁决，**不可改写绕过**）；未在白名单中的工具被拦截并显式失败回填到模型面，驱动后续轮换技能"

**关键点**：allowed-tools 是**单调硬限制**（用 Guard 而非 Hook），不能用普通业务钩子跳过。

### 3.2 预留环节

注释（react.go:44-45）：

> "预留 HookRequest / HookToolPreExecute / HookTurnStopping 环节供平台扩展"

具体接线点由驱动实现（react/planexecute/...）负责 —— 平台想加"tool 调用前权限检查"之类，可以注册到 `HookToolPreExecute`。

---

## 4. 主循环 `Run`（react.go:97-164）

### 4.1 前置校验 + 裸实例兜底

```go
if rt == nil || rt.Session == nil { return "", fmt.Errorf("agent: run requires session") }
if rt.Tools == nil { return "", fmt.Errorf("agent: no tool registry configured") }
if rt.Planner == nil { return "", fmt.Errorf("agent: no planner configured") }

// 裸实例惰性默认装配：Router 等场景直接构造 ReActDriver 时，Run 前补全默认钩子/守卫
if rt.HookBus == nil || rt.GuardBus == nil { NewReAct(rt) }
```

注释（react.go:107-108）说明兜底逻辑的必要性：

> "Router 等场景直接构造 ReActDriver 时，Run 前补全默认钩子/守卫（react.New 等显式装配入口调用后二者皆非 nil，此处跳过）"

**这是给"绕过 NewReAct 直接 new ReAct{}"的代码留的兜底** —— 减少心智负担，但显式调用 `NewReAct` 仍是推荐做法。

### 4.2 Sandbox 注入 ctx

```go
ctx = sandbox.WithRunner(ctx, rt.Runner)
```

把沙箱执行器注入 ctx，让下游工具/钩子通过 ctx 拿到 Runner（capability seam）。

### 4.3 任务状态初始化

```go
if sess.State.Goal == "" {
    sess.State.Goal = input  // 首次运行记录原始目标
}
if sess.State.Status != session.StatusRunning {
    sess.State.Status = session.StatusRunning  // 进入 running
}
```

注释（react.go:120）说明 Resume 路径：

> "（resume 路径由 Handle.Resume 预加载持久化的 TaskState，Goal 已非空。）"

也就是说：**resume 时 Goal 已经持久化了**，这里不会覆盖。

### 4.4 M2.8 resume 分叉

```go
firstTurn := sess.NextTurn()
if ckpt := rt.ResumeCheckpoint; ckpt != nil {
    firstTurn = ckpt.Turn   // 沿用挂起轮的 Turn
} else {
    sess.Append(event.SessionEventTurnStart, ...)
    sess.AppendUserMessage(input, "user")
}
```

注释（react.go:128-129）：

> "M2.8 resume：从挂起轮续跑，不追加 turn/start 与 user/message（该轮事件已在挂起前落库；turn 计数器沿用挂起轮）"

两种路径明确分离：
- **首次 / 续 Run**：追加 turn/start + user/message
- **Resume**：直接用 checkpoint.Turn 起步，不写新事件（这些事件已经落库过）

### 4.5 轮次循环

```go
for turn := firstTurn; ; turn++ {
    if rt.MaxTurns > 0 && turn-firstTurn+1 > rt.MaxTurns {
        sess.State.Status = session.StatusBlocked
        _ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: turn, Reason: "max_turns"})
        return "", fmt.Errorf("agent: max turns %d exceeded", rt.MaxTurns)
    }
    if err := ctx.Err(); err != nil {
        return "", fmt.Errorf("agent: run canceled: %w", err)
    }
    reply, done, err := d.runTurn(ctx, rt, sess, turn, history)
    ...
    history = sess.Messages()  // 本轮工具执行后历史更新
}
```

**几个细节**：

1. **`MaxTurns` 计数基准是本次 Run 内**（`turn-firstTurn+1`），不是全局 —— 这样 resume 不会让旧的 MaxTurns 计数器立刻用完。注释（react.go:146-147）说明：
   > "按本次 Run 内的轮次数计数（续跑会话 Turn 全局递增，上限语义不变）"

2. **每轮重读 history**：注释（react.go:162）解释：
   > "本轮工具执行后历史更新（直接作答/停止分支已 done）"
   
   只有在 `done=false` 时才会重读（即"工具调用后还没收尾"的情况），避免重复读已收尾的消息。

3. **`ctx.Err()` 检查**：每轮先看是否取消（架构 v2 §3.3 cancel 语义）。

---

## 5. 单轮循环 `runTurnInner`（react.go:180-266）

这是 ReAct 一轮的完整流程，源码注释（react.go:179）一图概括：

> "一轮循环的实现（pre-step → 规划 → 工具/作答 → 停止裁决）"

### 5.1 流程图

```
runTurn
  └─ runTurnInner
       ├─ M2.8 resume？  ──→ runResumeTurn（直接执行工具批量，跳过 LLM）
       │
       ├─ rt.AssembleSystem → 本轮系统提示词
       ├─ msgs = [system] + history
       │
       ├─ pre-step 钩子（压缩 + RAG 注入）
       │     └─ 可改写 msgs
       │
       ├─ sess.Append(step/start)
       ├─ nextAction → 调用模型 → planner.Action
       ├─ sess.AppendAssistantMessage
       ├─ rt.MonitorUsage
       ├─ rt.Notify(EventStep)
       │
       ├─ calls == 0?
       │     └─ YES → sess.State.Status=Completed, 写 step/end + turn/end{done}
       │                通知 EventDone，返回 (action.Content, true)
       │
       ├─ isTruncatedFinish?
       │     └─ YES → fail-closed：每个 call 都显式写 tool/result（带截断错误）
       │                返回 ("", false) 进入下一轮
       │
       ├─ execpath.ExecuteBatch → outs / suspend
       │     └─ suspend != nil？ → saveCheckpoint + 返回 (suspend, false)
       │
       └─ finalizeToolBatch
```

### 5.2 Resume 优先

```go
if ckpt := rt.ResumeCheckpoint; ckpt != nil && ckpt.Turn == turn {
    return d.runResumeTurn(ctx, rt, sess, turn, step, ckpt)
}
```

注释（react.go:186）：

> "M2.8 resume：从挂起轮直接进入工具批量执行（跳过 LLM 调用与日志重放）"

resume 跳过整个 LLM 阶段，因为：
- 模型已经做过决策（`calls` 已存在 checkpoint 中）
- 模型消息事件已经落库过
- 只需要按 checkpoint 的审批决策（`Decisions[call.ID]`）放行工具

### 5.3 系统提示词每轮刷新

```go
sysText, err := rt.AssembleSystem(ctx)
msgs = append(msgs, session.SystemMsg(sysText), history...)
```

注释（react.go:191）：

> "每轮刷新系统提示词：状态槽位随 TaskState 进度更新（M2.6 动态组装）"

**只有 system 不进日志**（react.go:142 注释："不含 system，每轮注入，不写日志"），因为 system 是"派生视图"而非"事实"。

### 5.4 截断检测：`isTruncatedFinish`（react.go:659-665）

```go
func isTruncatedFinish(reason string) bool {
    switch strings.ToLower(reason) {
    case "length", "max_tokens":
        return true
    }
    return false
}
```

注释（react.go:240-243）：

> "输出被截断（finish_reason=length/max_tokens）时，已产出的工具调用可能不完整，fail-closed：不执行、逐个显式失败回填（模型下一轮看到失败原因后调整），避免'已声明调用但模型面无回填'的静默丢弃"

**fail-closed 哲学**：截断时**不执行**工具调用，而是把"为什么没执行"作为 tool/result 回填给模型，让下一轮能调整（如减少输出、拆分任务）。

### 5.5 流式与非流式统一入口

```go
// nextAction
llmMsgs := session.ConvertToLlm(msgs)
tools := rt.Tools.List()
if !rt.Stream {
    return rt.Planner.NextAction(ctx, llmMsgs, tools)
}
sp, ok := rt.Planner.(planner.StreamingPlanner)
if !ok {
    return rt.Planner.NextAction(ctx, llmMsgs, tools)  // 规划器不支持流式 → 回退
}
return sp.NextActionStream(ctx, llmMsgs, tools, func(chunk llm.StreamChunk) error {
    if chunk.Delta == "" && chunk.Reasoning == "" { return nil }
    _ = sess.Append(event.SessionEventAssistantChunk, event.AssistantChunkData{...})
    rt.Notify(agent.Event{Type: agent.EventStream, ...})
    return nil
})
```

**三层回退**：
1. `rt.Stream == false` → 非流式
2. `Stream=true` 但规划器不是 `StreamingPlanner` → 回退非流式
3. 流式成功 → 边写 chunk 事件边发 stream 观测

### 5.6 工具批量：`execpath.ExecuteBatch`

```go
outs, suspend := execpath.ExecuteBatch(ctx, rt, sess, turn, step, calls)
```

注释（react.go:255-256）：

> "工具调用：批量执行（并发 + 按源序回填，架构 v2 §3.9 ConcurrentExecutor 三阶段）"

完整逻辑已经迁到 `execpath` 包（react.go:376-377 注释："工具批量执行已迁至 agent/execpath（共享通道）：react 批量与 scripted 各波共用同一 GuardBus / 审批挂起 / 钩子链 / 截断 / 审计 / tool/call 日志语义"）。

**返回值两套**：
- `outs []Outcome`：正常执行结果（含错误）
- `suspend *SuspendError`：异步审批挂起，需要落 checkpoint

### 5.7 resume 路径：`runResumeTurn`（react.go:271-290）

```go
_ = sess.Append(event.SessionEventCheckpointRestore, event.CheckpointData{
    Turn: turn, Step: step,
    HookKind:   string(ckpt.HookKind),
    DecisionOK: ckpt.ResolvedAt != nil,  // 已解决 = true（用于审计区分）
})
outs, suspend := execpath.ExecuteBatch(ctx, rt, sess, turn, step, calls)
// 二次挂起时重写 checkpoint 并冒泡
```

注释（react.go:269-270）：

> "M2.8 恢复路径：跳过 LLM 调用与日志，从 checkpoint 重新执行挂起轮的工具批量（Calls），决策表（Decisions）经 rt.ResumeCheckpoint 注入 approval guard 放行"

**关键不变量**：resume 时所有"已经做过的事"都不重做，只重做挂起那一刻的工具执行。

### 5.8 收尾：`finalizeToolBatch`（react.go:295-339）

三段式：

```
1. Doom Loop 致命检查 → 任务 blocked，返回 doomErr
2. 工具内挂起检测 → saveCheckpoint + 返回 suspend
3. execpath.ApplyOutcomes → 按源序回填（tool/result 日志 + 观测）
4. step/end 落库
5. ShouldStopAfterTurn 外部裁决 → stopped 收尾
6. 返回 ("", false) 进入下一轮
```

注释（react.go:292-294）：

> "Doom Loop 致命检查 → 工具内挂起检测 → 日志回填（execpath.ApplyOutcomes）→ step/end → 停止裁决 → 完成"

---

## 6. Doom Loop 处理流程

Doom Loop 是 GuardBus 单调守卫（详见 `docs/agent/hook.md §6`），但**最终在 `finalizeToolBatch` 里识别**：

```go
for _, out := range outs {
    var doomErr *hook.DoomLoopError
    if errors.As(out.Err, &doomErr) {
        sess.State.Status = session.StatusBlocked
        _ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: turn, Step: step})
        return "", false, doomErr
    }
}
```

注释（react.go:296）：

> "Doom Loop 是致命守卫错误：任务中断（状态 blocked），不回填工具结果"

也就是说：
- 普通工具错误 → 回填 tool/result，模型下一轮看到错误
- Doom Loop → **不写 tool/result**，直接终止（避免无意义的"又失败了"消息污染上下文）

---

## 7. 异步审批流：`policyGuard`（react.go:457-506）

policyGuard 是 ReAct 最复杂的 Guard，三段式裁决：

```go
// 1. Policy 裁决
err := rt.Policy.Allow(ctx, call)
if err == nil { return nil }                      // 允许
if !errors.As(err, &reqErr) { return err }        // 硬拒绝（非 Required）

// 2. RequiredError：需要人工确认
if rt.Approval == nil {
    return fmt.Errorf("工具 %q 需要人工确认，但未配置 Approval（fail-closed）", call.Name)
}

// 3. resume 决策表（已审批过）
if ckpt := rt.ResumeCheckpoint; ckpt != nil {
    if d, ok := ckpt.Decisions[call.ID]; ok {
        if !d.Approved {
            return &ToolDeniedError{...}
        }
        return nil                                // 已审批放行
    }
}

// 4. 发起审批请求
id, err := rt.Approval.Request(ctx, reqErr.Request)
if im, ok := rt.Approval.(approval.Immediate); ok {  // 同步适配
    if d, found := im.Decision(ctx, id); found {
        if !d.Approved { return &ToolDeniedError{...} }
        return nil
    }
}

// 5. 异步 → SuspendError，driver 写 checkpoint 冒泡
return &approval.SuspendError{...}
```

**三种结果**：
- **nil** = 放行
- **`ToolDeniedError`** = 拒绝（模型下一轮看到）
- **`SuspendError`** = 异步审批，driver 落 checkpoint 后返回上层

注释（react.go:452-456）总结：

> "policyGuard 将 Policy 注入单调守卫链（工具调用前裁决，不可被 hook 改写绕过）。兼承载 M2.8 异步审批：策略返回 *approval.RequiredError（需人工确认）时，走 Approval.Request → 挂起（*approval.SuspendError）/ 立即决策（Immediate）/ resume 决策表放行（Checkpoint.Decisions[call.ID]）。**未配置 Approval 时对需确认操作 fail-closed 拒绝**"

---

## 8. Checkpoint 落库：`saveCheckpoint`（react.go:385-421）

三步走：

```go
// 1. 落库 approval/request 事件（durable 审计观测）
_ = sess.Append(event.SessionEventApprovalRequest, ...)

// 2. 写 checkpoint（如果配置了 Checkpoints）
if rt.Checkpoints != nil {
    ckpt := &checkpoint.Checkpoint{SessionID, Turn, Step, HookKind, Calls, Request,
        ApprovalID, SuspendedCallID, Decisions: map[...]{}, Snapshot: sess.State, CreatedAt}
    rt.Checkpoints.Save(ctx, ckpt)
    _ = sess.Append(event.SessionEventCheckpointSave, ...)
}

// 3. 任务状态经 Persistence.SaveState 落盘（跨进程 Resume 还原 Facts）
if p := sess.Persistence; p != nil {
    p.SaveState(sess.ID, sess.State)
}
```

注释（react.go:382-384）解释落库边界：

> "未配置 rt.Checkpoints 时仅记录 approval/request 事件（SuspendError 仍冒泡，无法跨进程恢复）"

**降级语义**：
- 配 `Checkpoints`：完整跨进程恢复能力
- 没配：事件日志还在，但挂起时刻无法跨进程恢复（Resume 必须同进程）

---

## 9. fresh-context 压缩：`compressHook`（react.go:514-552）

`pre-step` 环节的瀑布钩子，做上下文压缩：

```go
func compressHook(rt *driver.RunContext) hook.Hook {
    return func(ctx, ev, next) {
        // 1. 跳过条件
        if rt.Compactor == nil || ev.Turn <= 1 || len(ev.Messages) == 0 {
            return next(ctx, ev)
        }
        
        // 2. 拆分 system + history
        sess := rt.Session
        msgs := ev.Messages
        var system []session.InternalMessage
        hist := msgs
        if msgs[0].Role == llm.RoleSystem {
            system = msgs[:1]
            hist = msgs[1:]
        }
        if len(hist) == 0 { return next(ctx, ev) }
        
        // 3. 调用 Compactor
        after, ok := rt.Compactor.Compact(hist, &sess.State)
        if !ok || len(after) == 0 { return next(ctx, ev) }
        
        // 4. 计算阴影范围（哪些旧消息被压了）
        seqs := session.MessageEventSeqs(sess.Events)
        shadowed := len(hist) - (len(after) - 1)
        var from, to int64
        if shadowed > 0 && shadowed <= len(seqs) {
            from = seqs[0]
            to = seqs[shadowed-1]
        }
        
        // 5. 三段式事件落库（start → summary → end）
        sess.Append(SessionEventCompactionStart, ...)
        sess.Append(SessionEventCompactionSummary,
            event.CompactionSummaryData{Summary: after[0].Content, ShadowedFrom: from, ShadowedTo: to})
        sess.Append(SessionEventCompactionEnd, ...)
        rt.Notify(agent.Event{Type: agent.EventCompacted, Content: sess.State.Snapshot()})
        
        ev.Messages = append(system, after...)
        return next(ctx, ev)
    }
}
```

**关键设计点**：

1. **`ev.Turn <= 1` 跳过**：第一轮不压（没历史可压）
2. **拆分 system**：压缩不能动 system（保持稳定利于 cache）
3. **三段式事件**：start/summary/end 三条事件齐全，**summary 携带 ShadowedFrom/To 让回放可还原**（详见 session.md §5.3）
4. **`after` 格式**：约定为 `[summary_msg, kept_msg_1, ...]`，所以 `len(after) - 1` 是保留消息数

### 阴影范围推导

`len(hist) - (len(after) - 1)` = `压缩前的历史消息数 - 压缩后保留的历史消息数` = **被丢弃的消息数**。

`MessageEventSeqs` 返回产生模型面消息的事件序号列表（不含 assistant/chunk），前 N 个就是"被压"的。

---

## 10. RAG 注入：`ragInjectHook` + `retrieveAndInject`（react.go:557-654）

`pre-step` 环节的另一个瀑布钩子，按任务目标检索长期记忆：

```go
func ragInjectHook(rt *driver.RunContext) hook.Hook {
    return func(ctx, ev, next) {
        if rt.Retriever == nil { return next(ctx, ev) }  // 未配置则跳过
        
        // 1. 构造查询：优先用 State.Goal，回退用最近 user message
        query := rt.Session.State.Goal
        if query == "" {
            for i := len(ev.Messages) - 1; i >= 0; i-- {
                if ev.Messages[i].Role == llm.RoleUser && !ev.Messages[i].Summary {
                    query = ev.Messages[i].Content
                    break
                }
            }
        }
        extras, err := retrieveAndInject(ctx, rt, ev.Turn, query)
        if err != nil { return nil, err }
        if len(extras) > 0 {
            ev.Messages = append(ev.Messages, extras...)
        }
        return next(ctx, ev)
    }
}
```

注释（react.go:554-556）：

> "pre-step 环节的 RAG 检索注入：按任务目标检索长期记忆，经知识守门后以 CustomMsg（来源标注 [rag]）注入本轮模型输入（**不进日志**，每轮按最新知识重检）"

**关键设计**：

1. **不进日志**：每次检索结果都不一样，存进日志没意义（重放时还应该重检）
2. **每轮重检**：用最新知识（注释明确强调）
3. **CustomMsg 标注 `[rag]`**：翻译边界会把它转 user 角色 + 前缀 `[rag]`（详见 session.md §6.3）

### 10.1 三道守门：`retrieveAndInject`（react.go:584-635）

```
Retriever.Retrieve(query, limit)  → docs
   ↓
冲突裁决：同来源只留最高分分块
   ↓
按 Score 降序排序
   ↓
知识守门：< MinScore 的分块丢弃（全部丢则通知 EventKnowledgeGap）
   ↓
截断到 maxChunks
   ↓
[]InternalMessage{CustomMsg("rag", renderRetrieved(...))}
```

**每道守门的意义**：

| 阶段 | 守的是什么 |
|---|---|
| 冲突裁决 | 防"同一文件多块都被注入"造成噪声 |
| MinScore 守门 | 防低质量分块污染上下文 |
| maxChunks 截断 | 控 token 上限 |
| EventKnowledgeGap 观测 | 让平台知道"知识库空了"，便于补料 |

---

## 11. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **日志即事实** | `sess.Append` 先落盘再入内存 | 跨进程恢复可重放 |
| **截断 fail-closed** | `isTruncatedFinish` 显式失败回填 | 不静默丢弃已声明调用 |
| **Doom Loop 终止任务** | `finalizeToolBatch` 不回填 tool/result | 避免无意义失败消息污染上下文 |
| **resume 跳过 LLM** | `runResumeTurn` 不调规划器 | 不重做决策，省 token |
| **second-level 二次挂起重写 checkpoint** | `runResumeTurn` 重写路径 | 重复审批能继续 |
| **system 不进日志** | `Run` 不追加 system 事件 | system 是派生视图 |
| **MaxTurns 本次 Run 内计数** | `turn-firstTurn+1` | resume 不立刻耗尽计数器 |
| **RAG 注入不进日志** | `ragInjectHook` 仅改写 `ev.Messages` | 每轮重检无副作用 |
| **DoomLoopGuard 仅 ReAct** | `NewReAct` 才注册 | 脚本里重复调用合法 |
| **allowed-tools 硬限制** | `skillAllowListGuard` 用 Guard | 不可被业务钩子绕过 |
| **未配 Approval 时需确认操作 fail-closed** | `policyGuard` 早返回错误 | 缺能力不裸奔 |

---

## 12. 自定义扩展姿势

### 12.1 替换默认 Compactor

```go
rt.Compactor = myCompactor{}  // 任何实现 agent.Compactor 的类型
// 装配时 InstallDefaults 会跳过默认 NewDefaultCompactor
```

### 12.2 替换 RAG Retriever

```go
rt.Retriever = myRetriever{}
rt.RetrieverCfg = agent.RetrieverConfig{Limit: 16, MaxChunks: 5, MinScore: 0.6}
```

`ragInjectHook` 自动生效（`rt.Retriever != nil`）。

### 12.3 加业务钩子（如 tool 超时）

```go
rt.HookBus.On(hook.HookToolExecute, func(ctx, ev, next) {
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()
    return next(ctx, ev)
})
```

`HookToolExecute` 是 around dispatch，钩子可包 next 加超时。

### 12.4 加业务守卫（限流）

```go
rt.GuardBus.Add(func(ctx, ev) error {
    if ev.ToolCall != nil && isRateLimited(ev.ToolCall.Name) {
        return fmt.Errorf("rate limit hit")
    }
    return nil
})
```

### 12.5 关闭 Doom Loop（不推荐）

```go
rt.DoomLoopMaxRepeat = 0  // 不注册 DoomLoopGuard
```

注释（react.go:84）："0 = 不启用"。生产慎用，模型卡死会空转。

### 12.6 直接接管 Run（绕过 NewReAct）

```go
a := &driver.Agent{Driver: &react.ReActDriver{}, RT: rt}
a.Run(ctx, sess, input)  // Run 内有裸实例兜底，会自动 InstallDefaults + Doom Loop
```

适用于 Router 场景。

---

## 13. 一句话总结

**`agent/driver/react/react.go` 是 MoS 的"默认智能体循环"：用三个层叠函数（`InstallDefaults` / `NewReAct` / `New`）覆盖"裸复用 / 完整默认 / 业务直用"三种装配场景，用 `Run` + `runTurnInner` + `runResumeTurn` + `finalizeToolBatch` 四层循环结构把"主循环 / 单轮 / resume 恢复 / 收尾"职责干净切开，用 `compressHook` + `ragInjectHook` + `execpath` 共享钩子 + `policyGuard` + `skillAllowListGuard` + `DoomLoopGuard` 组成「可改写环节 + 不可绕过守卫」的完整防御体系，用截断 fail-closed + Doom Loop 任务终止 + 二次挂起重写 checkpoint 三条不变量把"健壮性"钉死在主路径上。**