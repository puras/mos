# ScriptedDriver —— 脚本化编排驱动

> 来源：`agent/driver/scripted/` —— `scripted.go`（驱动主循环 + 合成轮 + checkpoint）+ `dag.go`（DAG 分波执行 + 插值 + 状态回填）+ `validate.go`（脚本校验：环检测、参数名校验、依赖一致性）。
> 目标读者：理解"模型写脚本，分波有界并发执行"的所有人；想自定义 ScriptPlanner / 步骤插值语义的开发者。

---

## 1. 一句话定位

**`ScriptedDriver` 是 MoS 的"写脚本（DAG）→ 分波有界并发执行（纯工具，0 次额外模型调用）→ 合成轮（模型产出最终回复）"编排驱动**：模型先写一份"步骤依赖图"，每步插值上游输出作为参数，**分波并发执行**（拓扑序），全部完成后再调一次模型产出回复。**全程仅 2 次模型调用**（写脚本 + 合成），中间是纯工具调度。

源码顶端的注释（scripted.go:1-4）一段话讲清定位：

> "Package scripted 提供脚本化编排驱动（架构 v2 §3.3 PTC 模式）：**写脚本（依赖 DAG）→ 分波有界并发执行（纯工具调用，0 次额外模型调用）→ 合成最终回复**。**全程 2 次模型调用**；工具执行经 agent/execpath 共享通道（与 react 批量语义一致）。"

文件分布：

| 文件 | 行数级 | 主题 |
|---|---|---|
| `scripted.go` | ~370 行 | 驱动主循环 + 合成轮 + checkpoint 挂起恢复 |
| `dag.go` | ~330 行 | DAG 分波执行 + 状态回填 + 步骤插值 + markSkipped |
| `validate.go` | ~135 行 | 脚本校验（步数 / ID / 工具存在 / 参数名 / 无环 / 命名唯一） |

---

## 2. 整体流程

```
Run(ctx, rt, input)
   ├─ 脚本阶段（1 次模型调用）
   │     ├─ ScriptPlanner.Script(input, msgs, tools) → ScriptedPlan{Steps, DependsOn, Args, Output}
   │     ├─ validateScript(&plan, tools, maxSteps)     ← fail-closed 校验
   │     ├─ newState(&plan)                            ← Status 全 pending
   │     ├─ 写 script/start + EventScript + assistant message（脚本文本）
   │     └─ 写每个 step pending 事件
   ├─ 执行阶段（0 次模型调用）
   │     └─ executeWaves(ctx, rt, plan, st)
   │           ├─ 拓扑分波：每波 = 依赖已满足的 pending steps
   │           ├─ resolveCall：插值 args → llm.ToolCall{ID: "step:<id>"}
   │           ├─ execpath.ExecuteBatch（复用 react 批量语义）
   │           ├─ applyOutcomes：Status（pending → running → done/failed/denied/skipped）
   │           └─ Outputs 累积 + tool/result 日志
   ├─ 挂起点（如触发 Approval）
   │     └─ saveCheckpoint → checkpoint {ScriptState + Calls + ApprovalID + TaskState}
   ├─ 合成阶段（1 次模型调用）
   │     ├─ nextAction（不注入工具，防模型继续调用）
   │     ├─ action.Content 写 assistant/message + EventStep
   │     ├─ 仍有 tool calls → fail-closed 拒绝（"synthesis must answer directly"）
   │     └─ 写 script/end + turn/end + EventDone
   └─ 返回 action.Content
```

**关键指标**：

| 指标 | 值 | 说明 |
|---|---|---|
| 模型调用次数 | **2 次** | 写脚本 + 合成 |
| 工具调用 | 任意（受 maxSteps 限制） | 脚本步骤数 ≤ DefaultMaxSteps=16 |
| 审批挂起点 | 任意 | 每步都可能触发 Approval → SuspendError |

---

## 3. `ScriptedDriver` 结构 + Option

```go
const DefaultMaxSteps = 16

type ScriptedDriver struct {
    planner  planner.ScriptPlanner
    maxSteps int
}

type Option func(*ScriptedDriver)
```

| Option | 默认 | 用途 |
|---|---|---|
| `WithPlanner(planner.ScriptPlanner)` | `rt.ScriptPlanner` | 注入脚本规划器（**均未设置时 Run 拒绝运行**） |
| `WithMaxSteps(int)` | `DefaultMaxSteps=16` | 覆盖脚本步数上限 |

**`DefaultMaxSteps=16` 注释**（scripted.go:30）：

> "防脚本爆炸 + 防提示词膨胀"

—— 模型可能写出"10 步调用 read_file 把同一文件读 16 遍"这种脚本，限制步数防止资源浪费。

---

## 4. `New` 装配（scripted.go:55-62）

```go
func New(rt *driver.RunContext, opts ...Option) *driver.Agent {
    d := &ScriptedDriver{maxSteps: DefaultMaxSteps}
    for _, o := range opts { o(d) }
    react.InstallDefaults(rt)   // ← 复用 react 默认钩子
    return driver.New(rt, d)
}
```

**与 react/planexecute/reflection 的关键差异**（注释 scripted.go:51-54）：

> "**不注册 Doom Loop 守卫**（脚本中故意重复同一工具调用是合法语义）；规划器取 WithPlanner 或 rt.ScriptPlanner，**均未设置时由 Run 阶段 fail-closed 拒绝运行**。"

—— script 驱动复用 `react.InstallDefaults`（不调 `react.NewReAct`，避免创建 ReActDriver 实例），但**跳过 DoomLoopMaxRepeat**（脚本里 read_file 16 次是合法的）。

---

## 5. `Run` 主循环（scripted.go:65-170）

### 5.1 初始化与 fail-closed 校验（scripted.go:66-81）

```go
if rt == nil || rt.Session == nil { return "", errors.New("agent: run requires session") }
if rt.Tools == nil { return "", errors.New("agent: no tool registry configured") }
if rt.Planner == nil { return "", errors.New("agent: scripted driver requires a Planner") }
if d.planner == nil && rt.ScriptPlanner == nil {
    return "", errors.New("agent: scripted driver requires a ScriptPlanner")
}
if rt.HookBus == nil || rt.GuardBus == nil {
    react.InstallDefaults(rt)   // 裸实例惰性兜底
}
```

**四个 fail-closed 检查**：

| 检查 | 含义 |
|---|---|
| Session / Tools 不为 nil | 跑 Run 前提 |
| **rt.Planner 不为 nil** | 合成轮需要 Planner |
| **ScriptPlanner 必须有** | 脚本阶段需要 ScriptPlanner |
| HookBus / GuardBus | 裸实例惰性兜底 |

### 5.2 Resume vs 首次运行（scripted.go:92-136）

```go
firstTurn := sess.NextTurn()
var plan planner.ScriptedPlan
var st *scriptState
var resumeCalls []llm.ToolCall

if ckpt := rt.ResumeCheckpoint; ckpt != nil {
    // resume 路径：从 checkpoint 恢复（脚本 + 状态 + 挂起波工具调用）
    firstTurn = ckpt.Turn
    _ = sess.Append(event.SessionEventCheckpointRestore, event.CheckpointData{...})
    if ckpt.ScriptState == nil {
        return "", errors.New("agent: scripted resume requires ScriptState in checkpoint")
    }
    plan = ckpt.ScriptState.Plan
    st = restoreState(&plan, ckpt.ScriptState)
    resumeCalls = ckpt.Calls
} else {
    // 首次路径：写脚本 + 校验 + 初始化
    _ = sess.Append(event.SessionEventTurnStart, event.TurnData{Turn: firstTurn, Reason: "user"})
    _ = sess.AppendUserMessage(input, "user")
    
    plan, err = d.makeScript(ctx, rt, sess, input)
    if err := validateScript(&plan, rt.Tools, d.maxSteps); err != nil {
        return "", fmt.Errorf("agent: script rejected: %w", err)
    }
    st = newState(&plan)
    _ = sess.Append(event.SessionEventScriptStart, event.ScriptStartData{...})
    rt.Notify(agent.Event{Type: agent.EventScript, ...})
    _ = sess.AppendAssistantMessage(firstTurn, scriptText(&plan), "", nil, llm.Usage{})
    for i := range plan.Steps {
        _ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{...})
    }
}
```

**Resume 路径关键**：

| 字段 | 来源 |
|---|---|
| `plan` | `ckpt.ScriptState.Plan` |
| `st` | `restoreState(&plan, ckpt.ScriptState)`（深拷贝 Outputs） |
| `resumeCalls` | `ckpt.Calls`（挂起波的工具调用） |
| `firstTurn` | `ckpt.Turn`（不重置 turn） |

### 5.3 执行阶段（scripted.go:138-156）

```go
var report *scriptReport
var suspend *approval.SuspendError
var execErr error
_ = telemetry.WithSpan(ctx, rt.Telemetry, telemetry.SpanDriverTurn,
    map[string]string{"turn": strconv.Itoa(firstTurn), "driver": "scripted"},
    func(ctx context.Context) error {
        report, suspend, execErr = executeWaves(ctx, rt, sess, firstTurn, &plan, st, resumeCalls)
        return nil
    })
if suspend != nil {
    if err := d.saveCheckpoint(ctx, rt, sess, firstTurn, &plan, st, suspend); err != nil {
        return "", fmt.Errorf("agent: save checkpoint: %w", err)
    }
    return "", suspend
}
```

**关键设计点**：

| 设计 | 含义 |
|---|---|
| **`telemetry.WithSpan` 包裹** | "scripted" driver 的 driver.turn span |
| **`executeWaves` 返回 (report, suspend, err)** | 三元组：执行报告 + 挂起 + 致命错误 |
| **挂起时 saveCheckpoint** | 写 script 状态 + 挂起波 calls + ApprovalID + TaskState |
| **挂起时 return suspend** | driver 冒泡 → Handle.Resume 续跑 |

### 5.4 script/end + 合成轮（scripted.go:158-169）

```go
scriptEnd := event.ScriptEndData{Turn: firstTurn, Status: "completed"}
if report != nil {
    scriptEnd.Steps = report.Status
    scriptEnd.Outputs = report.Outputs
}
_ = sess.Append(event.SessionEventScriptEnd, scriptEnd)
_ = sess.Append(event.SessionEventStepEnd, event.StepData{Turn: firstTurn, Step: 1})
_ = sess.Append(event.SessionEventTurnEnd, event.TurnData{Turn: firstTurn, Reason: "done"})

return d.synthesize(ctx, rt, sess)
```

—— script/end 载荷：**每步 Status + Outputs 摘要**（架构 v2 §6 契约）。

---

## 6. DAG 分波执行（dag.go）

### 6.1 `executeWaves` 主循环（dag.go:197-252）

```go
func executeWaves(ctx, rt, sess, turn, plan, st, resumeCalls) (*scriptReport, *approval.SuspendError, error) {
    // 1. Resume 重放
    if len(resumeCalls) > 0 {
        outs, suspend := execpath.ExecuteBatch(ctx, rt, sess, turn, 1, resumeCalls)
        if suspend != nil {
            st.ResumeCalls = resumeCalls
            return nil, suspend, nil
        }
        applyOutcomes(ctx, rt, plan, st, resumeCalls, outs, turn, sess)
        st.ResumeCalls = nil
    }

    // 2. 拓扑分波
    for wave := 0; wave <= len(plan.Steps); wave++ {
        if err := ctx.Err(); err != nil { return nil, nil, err }
        ready := readySteps(plan, st)
        if len(ready) == 0 { break }   // 无 ready → 全部处理完

        calls := make([]llm.ToolCall, 0, len(ready))
        callToStep := make(map[string]string, len(ready))
        for i := range ready {
            call, err := resolveCall(&ready[i], st.Outputs)
            if err != nil {
                st.Status[ready[i].ID] = statusFailed
                markSkipped(plan, st, ready[i].ID)
                continue
            }
            callToStep[call.ID] = ready[i].ID
            calls = append(calls, call)
        }
        if len(calls) == 0 { continue }   // 该波全部插值失败

        // 3. 标记 running + 写 script/step running 事件
        for _, c := range calls {
            id := callToStep[c.ID]
            st.Status[id] = statusRunning
            _ = sess.Append(event.SessionEventScriptStep, ...)
        }

        // 4. 并发执行（execpath.ExecuteBatch）
        outs, suspend := execpath.ExecuteBatch(ctx, rt, sess, turn, 1, calls)
        if suspend != nil {
            st.ResumeCalls = calls
            return nil, suspend, nil
        }

        // 5. 回填 Status + Outputs + tool/result 日志
        applyOutcomes(ctx, rt, plan, st, calls, outs, turn, sess)
    }

    // 6. 剩余 pending（不可达）→ skipped
    for i := range plan.Steps {
        if st.Status[plan.Steps[i].ID] == statusPending {
            st.Status[plan.Steps[i].ID] = statusSkipped
            _ = sess.Append(event.SessionEventScriptStep, ...)
        }
    }
    return &scriptReport{Status: st.Status, Outputs: st.Outputs}, nil, nil
}
```

### 6.2 六步骤拓扑分波

**步骤 1: Resume 重放**（dag.go:198-206）：

- 重放挂起波的工具调用（maxParallel=1，因为是恢复挂起波，保证顺序一致）
- 重放也挂起 → 记录 ResumeCalls 后再次返回 suspend（不会无限循环？注释里"挂起点持久化"会让 Handle.Resume 重新尝试）
- 重放成功 → 清空 ResumeCalls，进入正常分波

**步骤 2: `readySteps` 计算可执行步骤**（dag.go:255-273）：

```go
func readySteps(plan, st) []planner.ScriptStep {
    var out []planner.ScriptStep
    for _, s := range plan.Steps {
        if st.Status[s.ID] != statusPending { continue }
        ok := true
        for _, dep := range s.DependsOn {
            if st.Status[dep] != statusDone { ok = false; break }
        }
        if ok { out = append(out, s) }
    }
    return out
}
```

**关键设计**：

| 设计 | 含义 |
|---|---|
| **仅 pending 步骤考虑** | 已 done / running / failed / skipped 跳过 |
| **依赖全 done 才 ready** | 拓扑序保证 |
| **`ready == 0` 退出** | 全部处理完 |

**步骤 3: `resolveCall` 实例化**（dag.go:180-190）：

```go
func resolveCall(step *planner.ScriptStep, outputs map[string]any) (llm.ToolCall, error) {
    args, err := interpolateArgs(step.Args, outputs)
    if err != nil { return llm.ToolCall{}, err }
    b, err := json.Marshal(args)
    if err != nil { return llm.ToolCall{}, fmt.Errorf("script: step %q marshal args: %w", step.ID, err) }
    return llm.ToolCall{ID: "step:" + step.ID, Name: step.Tool, Args: string(b)}, nil
}
```

- **ID 前缀 `step:`**：让 ToolCall.ID 能反查是哪个 step（applyOutcomes 用 `strings.TrimPrefix(out.Input.ID, "step:")`）
- **插值失败 → statusFailed + markSkipped**：该波其他步骤继续，失败步骤下游全部 skipped

**步骤 4: 标记 running + 写事件**（dag.go:231-235）

**步骤 5: `execpath.ExecuteBatch`**（dag.go:236-241）：
- 复用 react 批量语义
- maxParallel=1（脚本每步并发度默认 1，**与 react 的 maxToolParallel 不同**——脚本是显式依赖图，不需要 4 路并发）
- **挂起时记录 ResumeCalls 返回 suspend**

**步骤 6: `applyOutcomes` 回填**（dag.go:278-299）：

```go
for i := range outs {
    out := outs[i]
    id := strings.TrimPrefix(out.Input.ID, "step:")
    switch {
    case out.Denied:
        st.Status[id] = statusDenied
        markSkipped(plan, st, id)
    case out.Output.Err != nil:
        st.Status[id] = statusFailed
        markSkipped(plan, st, id)
    default:
        st.Status[id] = statusDone
        st.Outputs[id] = out.Output.Result
    }
    _ = sess.Append(event.SessionEventScriptStep, event.ScriptStepData{
        Turn: turn, StepID: id, Tool: out.Input.Name, Status: st.Status[id],
        Detail: detailOf(out),
    })
}
_ = execpath.ApplyOutcomes(ctx, rt, sess, turn, calls, outs)
```

**三态分类**：

| 状态 | 触发 | 下游 |
|---|---|---|
| `statusDone` | 工具成功 + 无拒绝 | 正常累积 Outputs |
| `statusDenied` | preflight 拦截 | markSkipped |
| `statusFailed` | 工具执行 err | markSkipped |
| `statusSkipped` | 依赖 failed/denied | 不执行 |

**`markSkipped` 传递依赖**（dag.go:312-325）：

```go
func markSkipped(plan, st, stepID string) {
    if st.Status[stepID] != statusPending { return }
    st.Status[stepID] = statusSkipped
    for i := range plan.Steps {
        dep := &plan.Steps[i]
        for _, d := range dep.DependsOn {
            if d == stepID { markSkipped(plan, st, dep.ID) }
        }
    }
}
```

—— **递归传递**：一个 step failed/denied 后，所有依赖它的 step 全部 skipped（无论间接还是直接依赖）。

### 6.3 步骤状态机

```
pending ──(ready)──→ running ──(applyOutcomes)──→ done
   │                     │                        ↑
   │                     └──(ExecError)──→ failed
   │                     └──(Preflight 拦截)──→ denied
   │                                                
   └──(依赖 failed/denied)──→ skipped
```

**单向流转**：pending → running → done/failed/denied；pending → skipped（依赖传递）。

**为什么 failed/denied 步骤的"成功 Outputs"不写入？** 注释（dag.go:289）：

```go
default:
    st.Status[id] = statusDone
    st.Outputs[id] = out.Output.Result
```

—— 只在 default 分支（无 Denied 且无 Err）写 Outputs。failed/denied 不污染命名输出，避免下游步骤引用了"失败数据"。

---

## 7. 步骤参数插值（dag.go:60-177）

### 7.1 `stepRefRe` 引用语法

```go
var stepRefRe = regexp.MustCompile(`\$\{([A-Za-z0-9_-]+)(?:\.([A-Za-z0-9_]+))?\}`)
```

**两种引用形式**：

| 形式 | 含义 | 示例 |
|---|---|---|
| `${stepID}` | 上游完整结果 | `${read_file_result}` |
| `${stepID.field}` | 单层字段 | `${parse.user_id}` |

### 7.2 `interpolateArgs` 递归插值（dag.go:60-102）

```go
func interpolateArgs(args, outputs) (map[string]any, error) {
    if len(args) == 0 { return args, nil }
    out := make(map[string]any, len(args))
    for k, v := range args {
        rv, err := interpolateValue(v, outputs)
        if err != nil { return nil, err }
        out[k] = rv
    }
    return out, nil
}
```

**递归支持 map / slice / string**（`interpolateValue` 处理三种类型）。

### 7.3 `interpolateString` 两种模式（dag.go:104-147）

```go
func interpolateString(s, outputs) (any, error) {
    if !stepRefRe.MatchString(s) { return s, nil }

    // 整串恰好是一个引用 → 返回结构化值
    if m := stepRefRe.FindStringSubmatch(s); m != nil && m[0] == s {
        id, field := m[1], m[2]
        val, ok := outputs[id]
        if !ok { return nil, fmt.Errorf("script: unresolved step %q", id) }
        if field != "" { return lookupField(val, field) }
        return val, nil
    }

    // 含文本片段 → 字符串替换
    var b strings.Builder
    last := 0
    for _, idx := range stepRefRe.FindAllStringSubmatchIndex(s, -1) {
        b.WriteString(s[last:idx[0]])
        // ...
        last = idx[1]
    }
    b.WriteString(s[last:])
    return b.String(), nil
}
```

**两个分支**：

| 分支 | 触发 | 返回类型 |
|---|---|---|
| 整串是单个引用 | `"${id}"` 或 `"${id.field}"` | **结构化值**（保持 JSON 类型） |
| 含文本片段 | `"prefix ${id} suffix"` | **字符串替换后**（强制字符串） |

### 7.4 `lookupField` JSON 归一化（dag.go:152-166）

```go
func lookupField(val any, field string) (any, error) {
    b, err := json.Marshal(val)   // 强制 JSON 归一化
    if err != nil { return nil, fmt.Errorf("script: ${...}.%s: result not an object", field) }
    var m map[string]any
    if err := json.Unmarshal(b, &m); err != nil { return nil, ... }
    v, ok := m[field]
    if !ok { return nil, fmt.Errorf("script: ${...}.%s: field not found", field) }
    return v, nil
}
```

**关键设计**（注释 dag.go:149-151）：

> "统一经 JSON 归一化取字段：工具输出在模型面/参数面是 **JSON 形态，数字即 float64**；直接读 Go 原生 map 会保留 int 等类型，与后续参数序列化（json.Marshal）表现不一致。"

—— **跨层一致性**：JSON 进 JSON 出，避免 Go 原生类型污染。

---

## 8. `validateScript` 脚本校验（validate.go）

### 8.1 七项校验（validate.go:14-82）

```go
func validateScript(plan, reg, maxSteps) error {
    // 1. plan 不为 nil
    // 2. 步骤数 > 0
    // 3. maxSteps 限制
    // 4. ID 唯一非空
    // 5. 工具存在
    // 6. Output 命名唯一
    // 7. 参数名合法（schema 校验）
    // 8. DependsOn 已知 + 无自依赖 + 去重
    // 9. 环检测（Kahn）
}
```

### 8.2 七项校验详解

| 校验 | 实现 | 失败信息 |
|---|---|---|
| plan 不为 nil | `if plan == nil` | "nil plan" |
| 步骤数 > 0 | `len(plan.Steps) == 0` | "no steps" |
| maxSteps 限制 | `len(plan.Steps) > maxSteps` | "N steps exceeds max M" |
| ID 唯一非空 | `if st.ID == ""` / `if ids[st.ID]` | "step N: empty id" / "duplicate step id X" |
| 工具存在 | `reg.Get(st.Tool)` | "step X: unknown tool Y" |
| Output 命名唯一 | `if outputs[st.Output]` | "duplicate output name X" |
| 参数名合法 | `validateArgNames` 对照 schema.properties | "step X: unknown arg Y for tool Z" |
| DependsOn 已知 | `if !ids[dep]` | "step X depends on unknown step Y" |
| 无自依赖 | `if dep == st.ID` | "step X self-dependency" |
| DependsOn 去重 | `if seen[dep]` | "step X duplicate dependency Y" |
| 环检测 | `hasCycle(plan)` | "dependency cycle detected" |

### 8.3 `hasCycle` Kahn 拓扑排序（validate.go:103-132）

```go
func hasCycle(plan) bool {
    indeg := make(map[string]int, len(plan.Steps))
    adj := make(map[string][]string, len(plan.Steps))   // dep → 依赖它的 steps
    for i := range plan.Steps {
        st := &plan.Steps[i]
        indeg[st.ID] = len(st.DependsOn)
        for _, dep := range st.DependsOn {
            adj[dep] = append(adj[dep], st.ID)
        }
    }
    queue := make([]string, 0)
    for id, d := range indeg {
        if d == 0 { queue = append(queue, id) }
    }
    count := 0
    for len(queue) > 0 {
        id := queue[0]; queue = queue[1:]
        count++
        for _, n := range adj[id] {
            indeg[n]--
            if indeg[n] == 0 { queue = append(queue, n) }
        }
    }
    return count != len(plan.Steps)   // 未访问全部 → 有环
}
```

**Kahn 算法**：indegree 为 0 入队，逐个弹出并减邻居 indegree，最后访问数 ≠ 节点数 → 有环。

### 8.4 `validateArgNames` 参数名校验（validate.go:86-100）

```go
func validateArgNames(st, schema) error {
    if len(st.Args) == 0 { return nil }
    props, ok := schema["properties"].(map[string]any)
    if !ok { return nil }   // 宽松 schema（无 properties）跳过
    for k := range st.Args {
        if _, ok := props[k]; !ok {
            return fmt.Errorf("script: step %q: unknown arg %q for tool %q", st.ID, k, st.Tool)
        }
    }
    return nil
}
```

**关键设计**：

| 设计 | 含义 |
|---|---|
| **宽松 schema 跳过** | 工具没声明 properties → 执行期校验（不是脚本校验职责） |
| **只校验键名，不校验值** | 插值引用 `${...}` 出现在值里，校验时不展开 |

---

## 9. 状态 + 快照（dag.go + scripted.go）

### 9.1 `scriptState` 可恢复字段（dag.go:30-48）

```go
type scriptState struct {
    Status      map[string]string  // stepID → status
    Outputs     map[string]any     // stepID → 命名结果
    ResumeCalls []llm.ToolCall     // 挂起波的工具调用（resume 重放）
}
```

**字段全部导出**（注释 dag.go:30-32）：

> "字段导出：dag_test 的 TestRestoreStateDeepCopyOutputs 在恢复后 mutate Outputs/Status 断言与 checkpoint 快照的内存隔离（resume 续跑不污染存档）。"

### 9.2 `restoreState` 深拷贝（scripted.go:269-300）

```go
func restoreState(plan, ss) *scriptState {
    st := newState(plan)
    for id, s := range ss.Status { st.Status[id] = s }
    for id, v := range ss.Outputs { st.Outputs[id] = deepCopyValue(v) }
    return st
}

func deepCopyValue(v any) any {
    switch t := v.(type) {
    case map[string]any:
        out := make(map[string]any, len(t))
        for k, vv := range t { out[k] = deepCopyValue(vv) }
        return out
    case []any:
        out := make([]any, len(t))
        for i, vv := range t { out[i] = deepCopyValue(vv) }
        return out
    default:
        return v   // 标量不可变，复制安全
    }
}
```

**关键设计**（注释 scripted.go:266-268）：

> "**Outputs/Status 深拷贝**：不直接复用 checkpoint 的 map（MemStore.Load 是浅拷贝 `cp := *ckpt`），否则续跑期 mutate outputs/status 会改写存储的 ScriptState，**跨进程 Resume 读到旧值丢状态**。"

—— checkpoint 落盘后跨进程可能再被读，深拷贝切断引用关系。

### 9.3 `saveCheckpoint` 挂起点落库（scripted.go:303-339）

```go
ckpt := &checkpoint.Checkpoint{
    SessionID: sess.ID, Turn: turn, Step: 1,
    HookKind:        suspend.HookKind,
    Calls:           st.ResumeCalls,
    Request:         suspend.Request,
    ApprovalID:      suspend.ID,
    SuspendedCallID: suspend.CallID,
    Decisions:       make(map[string]approval.ApprovalDecision),
    Snapshot:        sess.State,
    ScriptState: &planner.ScriptState{
        Plan:    *plan,
        Outputs: st.Outputs,
        Status:  st.Status,
    },
    CreatedAt: time.Now(),
}
```

**关键字段**：

| 字段 | 来源 |
|---|---|
| `Calls` | `st.ResumeCalls`（挂起波工具调用，Resume 时重放） |
| `ScriptState.Plan` | 整份脚本（Resume 时重建 plan） |
| `ScriptState.Outputs` | 已完成步骤的命名输出（Resume 时恢复 Outputs） |
| `ScriptState.Status` | 各步骤 Status 映射（Resume 时恢复 Status） |
| `Snapshot` | TaskState 快照（Resume 时恢复 Facts/Status） |

—— Resume 时 `restoreState` + 重放 `ResumeCalls` + 继续 executeWaves。

---

## 10. `synthesize` 合成轮（scripted.go:190-244）

### 10.1 三大不变量

```go
// 1. 不注入工具（防模型继续调用）
hev := &hook.HookEvent{Kind: hook.HookPreStep, Messages: msgs, Turn: synTurn, Step: 1}

// 2. 仍有 tool calls → fail-closed 拒绝
if len(calls) > 0 {
    err := errors.New("agent: scripted synthesis must answer directly (no tool calls)")
    for i := range calls {
        _ = sess.AppendToolResult(synTurn, &calls[i], nil, "", err)
    }
    sess.State.Status = session.StatusBlocked
    return "", err
}

// 3. 空回复也失败
if action.Content == "" {
    sess.State.Status = session.StatusBlocked
    return "", errors.New("agent: scripted synthesis produced no reply")
}
```

**fail-closed 设计**：

| 场景 | 后果 |
|---|---|
| 模型在合成轮调工具 | fail-closed 拒绝（"synthesis must answer directly"） |
| 模型空回复 | fail-closed 拒绝（"produced no reply"） |
| 模型正常回复 | Completed + EventDone |

注释（scripted.go:189-190）：

> "合成轮：一次 NextAction（**不注入工具**，防模型继续调用）；模型仍返回工具调用时 fail-closed（**显式失败回填 + 报错**，不执行、不进多脚本循环）。"

—— 合成轮**只产出文本**，不允许再调工具（避免"写第二份脚本"）。

### 10.2 `nextAction` 流式路径（scripted.go:247-264）

```go
func nextAction(ctx, rt, sess, turn, msgs) (planner.Action, error) {
    llmMsgs := session.ConvertToLlm(msgs)
    if !rt.Stream {
        return rt.Planner.NextAction(ctx, llmMsgs, nil)
    }
    sp, ok := rt.Planner.(planner.StreamingPlanner)
    if !ok {
        return rt.Planner.NextAction(ctx, llmMsgs, nil)
    }
    return sp.NextActionStream(ctx, llmMsgs, nil, func(chunk llm.StreamChunk) error {
        if chunk.Delta == "" && chunk.Reasoning == "" { return nil }
        _ = sess.Append(event.SessionEventAssistantChunk, ...)
        rt.Notify(agent.Event{Type: agent.EventStream, ...})
        return nil
    })
}
```

—— 流式路径：模型支持时走 `NextActionStream`，**每个 chunk 写 assistant/chunk 事件 + EventStream**。与 react.runTurnInner 行为一致。

---

## 11. 与其他驱动的关系

| 维度 | ReActDriver | PlanExecuteDriver | ReflectionDriver | ScriptedDriver |
|---|---|---|---|---|
| 模型调用次数 | 任意（每轮 1 次） | N+1（1 计划 + N 步骤 × ReAct） | 1+2N（生成 + N 次 Revise 各 2 次） | **2（写脚本 + 合成）** |
| 任务循环 | 单轮循环 | 写计划 + 多步循环 | 生成 + 反思 + 修订 | 写脚本 + 分波 + 合成 |
| 工具调度 | execpath.ExecuteBatch | 内嵌 ReAct | 内嵌 ReAct | **execpath.ExecuteBatch（拓扑分波）** |
| 必需组件 | — | PlanPlanner（可选） | Reflector（必需） | **ScriptPlanner + Planner（必需）** |
| DoomLoop 守卫 | ✅ | ✅ | ✅ | ❌（脚本重复合法） |
| 共享 RT | ✅ | ✅ | ✅ | ✅ |
| Checkpoint 扩展点 | SuspendError | SuspendError | SuspendError | **SuspendError + ScriptState** |

**关键差异**：scripted 是**唯一带 ScriptState 扩展点**的驱动，checkpoint 多一个 `ScriptState *planner.ScriptState` 字段（详见 `docs/agent/checkpoint.md §3.3`）。

---

## 12. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **2 次模型调用** | 写脚本 + 合成 | PTC 模式 |
| **DefaultMaxSteps=16** | scripted.go:30 | 防脚本爆炸 |
| **不注册 Doom Loop** | scripted.go:51-54 注释 | 脚本重复合法 |
| **ScriptPlanner 必需** | Run 校验 | fail-closed |
| **Planner 必需** | Run 校验（合成轮） | fail-closed |
| **validateScript 7 项** | validate.go | 失败 fail-closed |
| **maxSteps 限制** | validate.go + WithMaxSteps | 平台可覆盖 |
| **拓扑分波** | readySteps | 依赖未满不可执行 |
| **依赖传递 skipped** | markSkipped 递归 | 一个失败下游全跳过 |
| **failed/denied 不写 Outputs** | applyOutcomes | 防污染下游 |
| **失败波记录 ResumeCalls** | executeWaves 挂起分支 | Resume 重放 |
| **深拷贝 Outputs** | restoreState + deepCopyValue | 跨进程 resume 安全 |
| **Checkpoint 含 ScriptState** | saveCheckpoint | 续跑必备 |
| **合成轮不允许工具调用** | synthesize fail-closed | 不进多脚本循环 |
| **整串单引用保持类型** | interpolateString 第一个分支 | JSON 类型一致 |
| **含文本片段变字符串** | interpolateString 第二个分支 | 模板插值 |
| **lookupField 经 JSON** | lookupField | 跨层类型一致 |

---

## 13. 自定义扩展姿势

### 13.1 自定义 ScriptPlanner（基于 LLM 生成脚本）

```go
type LLMScriptPlanner struct {
    model llm.ChatModel
}

func (p *LLMScriptPlanner) Script(ctx context.Context, input string, msgs []llm.Message, tools []llm.Tool) (planner.ScriptedPlan, error) {
    resp, err := p.model.Chat(ctx, &llm.Request{
        Messages: []llm.Message{
            {Role: llm.RoleSystem, Content: "你是一个脚本规划器。根据任务和可用工具，写一份依赖 DAG。"},
            {Role: llm.RoleUser, Content: fmt.Sprintf("任务：%s\n可用工具：%v", input, tools)},
        },
    })
    if err != nil { return planner.ScriptedPlan{}, err }
    return parseScript(resp), nil  // 解析 {Steps, DependsOn, Args, Output}
}

agent, _ := assistant.New(model, tools, assistant.Config{
    Driver: "scripted",
    ScriptPlanner: &LLMScriptPlanner{model: myModel},
    FileAccess: ...,
})
```

### 13.2 自定义最大步数

```go
agent, _ := assistant.New(model, tools, assistant.Config{
    Driver: "scripted",
    DriverFactory: func(rt *driver.RunContext) *driver.Agent {
        return scripted.New(rt, scripted.WithMaxSteps(32))  // 8 步 → 32 步
    },
})
```

### 13.3 自定义步骤插值语法

如需扩展 `${...}` 语法（如 `${id[0].field}`），需要 fork 整个 scripted 包 —— 注释（dag.go:149-151）的 JSON 归一化哲学（"跨层一致"）是关键。

### 13.4 自定义步骤校验（额外业务规则）

`validateScript` 是包私有，无法注入额外校验。需要 fork 或包级别注入 —— 后续可通过 `ScriptPlanner` 接口返回的 plan 已经过校验（fail-closed）。

---

## 14. 一句话总结

**`ScriptedDriver` 是 MoS 的"脚本化编排驱动（PTC 模式）"：模型写脚本（ScriptPlanner）→ 7 项 fail-closed 校验（步数/ID/工具/参数名/无环/Output 命名唯一）→ 拓扑分波有界并发执行（`executeWaves`，每波 = 依赖已满足的 pending，execpath.ExecuteBatch 复用 react 批量语义）→ 步骤参数插值（`${id}` 整串保类型 / `${id.field}` 单层字段 / 含文本片段变字符串 / JSON 归一化跨层一致）→ 失败 failed/denied 递归 markSkipped（不污染下游 Outputs）→ 挂起时 saveCheckpoint（ScriptState + ResumeCalls + TaskState）→ 合成轮 1 次模型调用（不注入工具，fail-closed 拒绝再调工具）。** 全程仅 2 次模型调用，是 v2 §3.3 PTC 模式 + §3.8 Checkpoint 扩展的最复杂落地。