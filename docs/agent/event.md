# 事件基础设施（agent/event）

> 来源：`agent/event/events.go`（会话事件目录 + 载荷类型） + `agent/event/eventbus.go`（EventBus 四模式分发）。
> 目标读者：理解"事件从哪里来、到哪里去"的所有人；扩展智能体观测/审计/审批/Checkpoint 的开发者。

---

## 1. 一句话定位

**`agent/event` 是 agent 子系统的"事件基础设施"**：一边定义**会话事件类型字典**（`SessionEvent*` 常量 + 对应载荷结构），另一边提供**EventBus 四模式分发器**（Emit/Waterfall/Serial/Parallel）。前者是"事实的形状"，后者是"事件的去向"。

文件分布：

| 文件 | 职责 |
|---|---|
| `events.go` | 会话事件类型常量（30+ 种）、typed payload 结构、`MarshalData` 序列化 helper |
| `eventbus.go` | `EventBus` 通用分发器、4 种 DispatchMode、`On` 注册 + 幂等移除 |

---

## 2. 两层事件的清晰边界

注释（events.go:9-11）一上来就讲清楚核心区分：

> "事件类型是字符串常量，与 Observer 的 EventType（int 观测枚举）相互独立：**会话事件是唯一事实来源（持久化/回放），观测事件是 emit 投影（平台观测）**"

| 维度 | 会话事件（`SessionEvent*`） | 观测事件（`agent.Event`） |
|---|---|---|
| 类型 | 字符串（`"turn/start"`） | int 枚举（`agent.EventStep`） |
| 流向 | 写入 Session.Events（append-only） | Emit 到 Observer / EventBus |
| 持久化 | 必须（durable） | 不必须（live） |
| 用途 | 重放、跨进程恢复、审计、回放 | UI 观测、平台监控 |
| 关系 | 写入时**伴随**发观测 | 观测不反过来影响日志 |

**这是两个独立系统**：会话事件是"事实"，观测事件是"投影"。代码里同时存在两者是 feature，不是冗余 —— 因为：

- **持久化的会话事件不能丢**，要保证 replay 完整
- **观测事件可以丢**，但要低延迟给 UI 看
- 平台可以**只订阅观测事件**而不读整个事件日志（性能）

---

## 3. 会话事件类型字典（架构 v2 §3.2 / §4.1 / §4.2）

按主题分组（共 30+ 种，events.go:12-72）：

### 3.1 任务结构（turn / step）

```go
SessionEventTurnStart         = "turn/start"
SessionEventTurnEnd           = "turn/end"
SessionEventStepStart         = "step/start"
SessionEventStepEnd           = "step/end"
```

**对应**：每次 ReAct 循环 / PlanExecute / Scripted / Reflection 的"开/关"边界。

### 3.2 用户与助手

```go
SessionEventUserMessage       = "user/message"
SessionEventAssistantChunk    = "assistant/chunk"     // 流式增量
SessionEventAssistantMessage  = "assistant/message"   // 完整消息
```

注释（events.go:94）解释 `AssistantChunkData` 的存在理由：

> "流式增量（token 级回放保真）"

也就是说，chunk 事件是**回放保真度**用的 —— 重放会话时能精确还原"打字机效果"和 reasoning 增量。

### 3.3 工具调用

```go
SessionEventToolCall    = "tool/call"
SessionEventToolResult  = "tool/result"
```

**载荷关键点**（events.go:118-119）：

> "Result 为原始返回值（审计/平台观测用），Text 为给模型看的文本（截断后）；deriveMessages 优先采用 Text"

| 字段 | 服务对象 | 用途 |
|---|---|---|
| `Result any` | 审计 / 平台 | 原始结构化数据，永久保存 |
| `Text string` | 模型 | 截断后的纯文本，进模型上下文 |
| `Error string` | 模型 | 工具执行错误时回填 |

**Result 与 Text 分离的妙处**：审计能查到完整原始数据（用于复现问题），模型只看到精简文本（节省 token）。

### 3.4 压缩（compaction）

```go
SessionEventCompactionStart   = "compaction/start"
SessionEventCompactionSummary = "compaction/summary"
SessionEventCompactionEnd     = "compaction/end"
```

三段式（详见 session.md §5.3）：

- **start** = 压缩开始
- **summary** = 携带 `ShadowedFrom/To` 标记被替换范围（**可回放**）
- **end** = 压缩结束

### 3.5 各驱动的领域事件

```go
// PlanExecute
SessionEventPlanStart    = "plan/start"
SessionEventPlanStep     = "plan/step"     // pending → running → done/failed
SessionEventPlanEnd      = "plan/end"      // completed / failed

// Scripted
SessionEventScriptStart  = "script/start"
SessionEventScriptStep   = "script/step"   // pending → running → done/failed/denied/skipped
SessionEventScriptEnd    = "script/end"    // completed / failed + Steps + Outputs

// Reflection
SessionEventReflectionStart  = "reflection/start"
SessionEventReflectionReview = "reflection/review"  // Issues 为空 = 评审通过
SessionEventReflectionEnd    = "reflection/end"     // completed / max_iterations / failed
```

注释（events.go:201）解释 ScriptEnd 的双重作用：

> "Steps / Outputs 是脚本执行报告（scriptReport）的可观测投影，UI 与审计可直接读取"

也就是说：**驱动专属事件 = 该驱动专属的报告**。比如 ScriptEnd 的 `Steps` 字段是 map[stepID]status，`Outputs` 是命名输出 —— 直接给 UI 当渲染数据用。

### 3.6 Agent 域（M2.8，§4.2）

```go
// 异步审批
SessionEventApprovalRequest  = "approval/request"   // 发起，挂起前 durable 落库
SessionEventApprovalResolve  = "approval/resolve"   // 回填，emit 观测

// 文件路径解析（file-tool-jail Task 19）
SessionEventFsResolve        = "fs/resolve"         // allowed/denied + 判定依据

// Checkpoint（M2.8）
SessionEventCheckpointSave     = "checkpoint/save"     // 挂起前落库
SessionEventCheckpointRestore  = "checkpoint/restore"  // 恢复后回写

// Agent 句柄生命周期
SessionEventAgentCreated   = "agent/created"     // Registry.Create
SessionEventAgentDisposed  = "agent/disposed"    // Handle.Dispose
SessionEventAgentStatus    = "agent/status"      // running/idle/blocked/suspended

// 占位
SessionEventSubagentStart  = "subagent/start"    // M3 规划
SessionEventAgentInbox     = "agent/inbox"       // M3 Inbox 规划

// Skill
SessionEventSkillLoad        = "skill/load"          // 清单装载
SessionEventSkillActivate    = "skill/activate"      // 激活
SessionEventSkillDeactivate  = "skill/deactivate"    // 取消激活
```

**几个值得品的细节**：

1. **`approval/request` 是 durable 而 `approval/resolve` 是 emit** —— 注释（events.go:42-43）解释：
   > "仅作观测/审计；权威状态以 checkpoint.Decision 字段为准"

   也就是说：**resolve 事件丢了不影响续跑**（决策持久化在 Checkpoint.Decisions map 里），只是审计会少一条。

2. **`fs/resolve` 是 fs capability 层的路径裁决**（events.go:44-47）：
   > "文件工具经 fs capability 解析路径时发出，记录 allowed/denied 与判定依据，使'哪些路径被拒绝、为什么'可回放"

   与 shell 侧 Policy 审计**互补**：fs 层管"路径合法吗"，Policy 层管"命令合法吗"。

3. **`agent/created` / `agent/disposed` 对应 Handle 生命周期**（events.go:54-57）—— Registry.Create 是入口，Handle.Dispose 走 Scope.Close。

---

## 4. 事件载荷：`MarshalData` helper

```go
func MarshalData(v any) json.RawMessage {
    b, err := json.Marshal(v)
    if err != nil {
        return json.RawMessage(`{}`)
    }
    return json.RawMessage(b)
}
```

简单包装，把任意结构编码为 `json.RawMessage` 给 `SessionEvent.Data`。

**两个细节**：

1. **`json.RawMessage` 而不是 `[]byte`** —— 类型系统告诉 reader "这是合法 JSON"，下游可以直接 `json.Unmarshal(ev.Data, &d)`。
2. **失败兜底为 `{}`** —— Marshal 几乎不会失败（payload 都是简单结构），但兜底避免 panic。

---

## 5. EventBus 四模式分发（架构 v2 §3.1）

源码（eventbus.go:11-23）的注释把四种模式一句话讲清：

```go
const (
    ModeEmit       DispatchMode = iota  // 观察：无返回值
    ModeWaterfall                       // 包装：返回值向下传播，可短路
    ModeParallel                        // 扇出：并发执行
    ModeSerial                          // 裁决：任一返回 false 即短路
)
```

### 5.1 四模式对照表

| 模式 | 调用方式 | 返回值用途 | 短路语义 | 典型场景 |
|---|---|---|---|---|
| **Emit** | 串行 | 忽略 | 监听器 error → 中断后续 | 日志、UI 观测 |
| **Waterfall** | 串行 | 下一个的输入 | `(nil, nil)` = 短路保持当前 | 消息链改写 |
| **Parallel** | 并发 | 忽略 | 聚合首个 error | 并行观测 |
| **Serial** | 串行 | 断言为 bool | 任一 false → 整体 false | 策略裁决 |

### 5.2 `EventHandler` 统一签名

```go
type EventHandler func(ctx context.Context, payload any) (any, error)
```

**返回值的语义按模式而变**（eventbus.go:26-28）：

> "Emit/Parallel 模式下返回值被忽略；Waterfall 模式下返回值（any）作为下一监听器输入；Serial 模式下返回的 bool 为裁决结果，error 表示处理失败"

这就是为什么一个签名能支持四种模式 —— **用同一函数表达，由调用方按模式解读**。

### 5.3 数据结构

```go
type EventBus struct {
    mu        sync.RWMutex
    listeners map[string][]eventListener
    nextID    uint64
}

type eventListener struct {
    id uint64
    h  EventHandler
}
```

按 `name` 分组的有序列表，配 `id` 实现**幂等移除**（同 HookBus 的 On）。

### 5.4 `On` 注册 + 幂等移除（eventbus.go:48-73）

```go
func (b *EventBus) On(name string, mode DispatchMode, h EventHandler) func()
```

与 HookBus.On 完全对称的设计：`sync.Once` 包裹的移除函数。注释风格、返回值用法、并发模型都一样 —— 这俩包是孪生设计。

### 5.5 `snapshot` 取快照（eventbus.go:154-161）

```go
func (b *EventBus) snapshot(name string) []eventListener {
    if b == nil { return nil }
    b.mu.RLock()
    defer b.mu.RUnlock()
    return append([]eventListener(nil), b.listeners[name]...)
}
```

**关键设计**：快照在锁内完成、返回独立 slice，然后立即释放锁。后续分发**不持锁**，允许监听器内回调 `On` 注册新监听器（虽然不会影响本次分发，但不会死锁）。

这与 HookBus.Waterfall 的快照策略一致 —— 见 `docs/agent/hook.md §3.4`。

### 5.6 各模式实现细节

#### 5.6.1 Emit（eventbus.go:76-84）

```go
func (b *EventBus) Emit(ctx context.Context, name string, payload any) error {
    ls := b.snapshot(name)
    for _, l := range ls {
        if _, err := l.h(ctx, payload); err != nil {
            return err  // 任一失败即中断
        }
    }
    return nil
}
```

**短路语义**：监听器返回 error 就停，后续不执行。适合"日志链路"—— 一个挂了不能假装后面也成功了。

#### 5.6.2 Waterfall（eventbus.go:88-102）

```go
func (b *EventBus) Waterfall(ctx context.Context, name string, payload any) (any, error) {
    ls := b.snapshot(name)
    cur := payload
    for _, l := range ls {
        out, err := l.h(ctx, cur)
        if err != nil {
            return nil, err
        }
        if out == nil {
            return cur, nil  // 短路：返回当前值
        }
        cur = out
    }
    return cur, nil
}
```

**两个细节**：

1. **`out == nil` 表示短路**：监听器用 `(nil, nil)` 显式表达"我不改，下一个也别再改" —— 但其实下一个不会再被调用，所以这个判断有点反直觉
2. **payload 沿链传播**：每次返回值赋给 `cur`，下一个监听器拿到的是改写后的值

#### 5.6.3 Serial（eventbus.go:106-122）

```go
func (b *EventBus) Serial(ctx context.Context, name string, payload any) (bool, error) {
    ls := b.snapshot(name)
    for _, l := range ls {
        out, err := l.h(ctx, payload)
        if err != nil { return false, err }
        ok, isBool := out.(bool)
        if !isBool {
            return false, fmt.Errorf("eventbus: %q serial listener must return bool", name)
        }
        if !ok {
            return false, nil  // 任一 false 即整体 false
        }
    }
    return true, nil
}
```

**强类型断言**：监听器返回值必须能断言为 `bool`，否则报错（不静默吞错 —— fail-fast）。

#### 5.6.4 Parallel（eventbus.go:125-144）

```go
func (b *EventBus) Parallel(ctx context.Context, name string, payload any) error {
    ls := b.snapshot(name)
    if len(ls) == 0 { return nil }
    errs := make(chan error, len(ls))
    var wg sync.WaitGroup
    for _, l := range ls {
        wg.Add(1)
        go func(h EventHandler) {
            defer wg.Done()
            if _, err := h(ctx, payload); err != nil {
                errs <- err
            }
        }(l.h)
    }
    wg.Wait()
    close(errs)
    return errors.Join(collectErrors(errs)...)
}
```

**几个细节**：

1. **buffered channel** `errs := make(chan error, len(ls))` —— 容量等于监听器数，**保证不阻塞 send**（即使没人立即收）
2. **`errors.Join`** —— Go 1.20+ 聚合多个错误，让上游能拿到全部失败原因
3. **空监听器短路** —— `len(ls) == 0` 直接返回，避免起 goroutine 的开销

---

## 6. EventBus 与 HookBus 的对照

| 维度 | HookBus | EventBus |
|---|---|---|
| 签名 | `Hook(ctx, ev, next)` / `Guard(ctx, ev) error` | `EventHandler(ctx, payload) (any, error)` |
| 环节定义 | `HookKind` 6 种固定枚举 | `name string` 任意字符串 |
| 执行模式 | 仅 waterfall | 4 种（emit/waterfall/parallel/serial） |
| 短路 | 不调 next / 返回 error | 视模式而定 |
| 用途 | 智能体循环环节改写 | 通用事件分发 |
| 是否带 next | Hook 有，Guard 没有 | 无 next，统一靠返回值 |

**HookBus 是 EventBus 的特例**（限定 6 种 kind、固定 waterfall）。EventBus 是更通用的"扩展点"。

---

## 7. 与其它模块的协作边界

### 7.1 event 包不依赖 driver

只依赖：
- `llm`（`ToolCall`、`Usage`）

**不依赖** `agent/driver`、`session`、`hook`。是真正的"底层基础设施"。

### 7.2 session 包消费 event 包

```go
// session.go:73-91
ev := SessionEvent{Seq: ..., Type: typ, Data: event.MarshalData(data)}
```

`MarshalData` 是 event 包对外的关键 helper —— session 包用它把 typed payload 序列化成 `json.RawMessage`。

### 7.3 driver 包既产出会话事件也消费事件名

```go
// react.go:142
firstTurn := sess.NextTurn()  // 扫 turn/start 事件
```

`sess.NextTurn` 扫 `SessionEventTurnStart` 事件找最大 Turn —— 这是事件驱动编程的典型用法（详见 session.md §4.4）。

---

## 8. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **会话事件与观测事件分离** | 字符串 vs int 类型 | 持久化与观测解耦 |
| **Result 与 Text 双轨** | `ToolResultData` 两字段 | 审计保真 + 模型瘦身 |
| **三段式压缩事件可回放** | `ShadowedFrom/To` 写入 summary | 摘要替换语义可重放 |
| **resume 与 done 在 durable 上不可混** | approval/request 是 durable、resolve 是 emit | 权威状态走 Checkpoint |
| **On 注册幂等** | `sync.Once` 包裹 | 资源管理安全 |
| **snapshot 不持锁** | 拷贝 entries 后立即 RUnlock | 防监听器死锁 |
| **nil bus 全 no-op** | `b == nil` 分支 | 装配友好 |
| **Parallel 不阻塞 send** | buffered channel | goroutine 安全 |
| **errors.Join 聚合错误** | Parallel 实现 | 上游拿到完整失败原因 |
| **Wat
erfall 短路靠 `nil` 返回** | `out == nil` 判断 | 监听器显式表达"不改" |

---

## 9. 自定义扩展姿势

### 9.1 注册观测监听器（Emit 模式）

```go
remove := rt.Bus.On("observer/event", event.ModeEmit, func(ctx, payload) (any, error) {
    ev := payload.(agent.Event)
    log.Printf("[%s] turn=%d %s", ev.Type, ev.Turn, ev.Content)
    return nil, nil
})
defer remove()
```

### 9.2 写一条自定义事件

```go
// 直接走 Session 持久化
sess.Append(event.SessionEventCheckpointSave, event.CheckpointData{
    Turn: 1, Step: 1, HookKind: "tool/pre-execute",
})

// 或只是 emit 观测（不持久化）
rt.Bus.Emit(ctx, "my/observer/event", myPayload)
```

### 9.3 用 Waterfall 改写载荷

```go
rt.Bus.On("message/before-send", event.ModeWaterfall, func(ctx, payload) (any, error) {
    msgs := payload.([]llm.Message)
    // 改写 msgs
    return msgs, nil
})
```

### 9.4 用 Serial 做策略裁决

```go
rt.Bus.On("policy/check-tool", event.ModeSerial, func(ctx, payload) (any, error) {
    call := payload.(*llm.ToolCall)
    return isAllowed(call.Name), nil  // 必须返回 bool
})
```

### 9.5 用 Parallel 并行观测

```go
rt.Bus.On("metrics/emit", event.ModeParallel, func(ctx, payload) (any, error) {
    return nil, sendToPrometheus(payload)
})

rt.Bus.On("audit/log", event.ModeParallel, func(ctx, payload) (any, error) {
    return nil, writeAuditLog(payload)
})
```

两个监听器**并发执行**（互不阻塞），错误聚合返回。

---

## 10. 一句话总结

**`agent/event` 用「会话事件字典（持久化事实）+ EventBus 四模式分发器（观测扩展点）」两层设计把"事件"这件事切成两个独立系统：会话事件是事实（30+ 种 typed payload 严格对应 session.Append 写入），EventBus 是去往（4 种模式按场景选用，Emit 给观测、Waterfall 给改写、Parallel 给并行、Serial 给裁决）。**两者通过 `MarshalData` helper 与"`SessionEvent*` 字符串常量"互相耦合，但语义边界清晰 —— 这是"日志即事实 + 事件即扩展点"双重哲学在底层的具体落地。**
