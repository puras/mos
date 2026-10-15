# Agent 生命周期句柄（agent/handle）

> 来源：`agent/handle/handle.go`（AgentHandle + Registry） + `agent/handle/missing.go`（资源校验）。
> 目标读者：理解"Agent 怎么被创建 / 续跑 / 释放"的工程师；构建长任务平台的开发者。

---

## 1. 一句话定位

**`agent/handle` 是 Agent 的生命周期管理层**：用 `AgentHandle` 把"会话 + Agent + RunContext + Scope + Store"五个对象绑成一个**会话级句柄**，用 `Registry` 管理会话 ID → 句柄的映射；提供 `Create` / `Resume` / `Dispose` 三个核心动作，让平台像管理 HTTP 连接一样管理智能体任务。

源码顶端的注释（handle.go:1-4）一段话讲清定位：

> "提供 Agent 生命周期句柄（架构 v2 §3.8，对齐 dsh AgentHandle）：create / resume / dispose。Registry 管理会话 ID → AgentHandle 映射，Resume 从持久化重放会话 + checkpoint 恢复挂起轮（M2.8 异步审批续跑）"

文件分布：

| 文件 | 职责 |
|---|---|
| `handle.go` | `AgentHandle`（单会话句柄）+ `Registry`（会话 ID → 句柄映射） |
| `missing.go` | `Missing` 错误 + `ValidateMissing`（resume 前资源校验） |

---

## 2. 整体架构

```
Registry (并发安全 map)
   │
   ├─ newAgent: NewAgent 回调（由 assistant 提供，构造 *driver.Agent + *RunContext）
   ├─ store:    checkpoint.Store（持久化挂起点）
   └─ bus:      *event.EventBus（可选：广播 agent/created / agent/disposed 等）
   │
   ▼
AgentHandle (会话 ID → 句柄)
   ├─ ID:      会话 ID
   ├─ Session: *session.Session
   ├─ Agent:   *driver.Agent
   ├─ RT:      *driver.RunContext
   ├─ Scope:   *scope.Scope（资源作用域，逆序 Close）
   ├─ Store:   checkpoint.Store（与 Registry 共享）
   └─ disposed: bool（幂等保护）
```

**关键设计**：

- **`AgentHandle` 是一个会话的完整状态** —— 不需要外界再传任何东西进来，调用方只用 `handle.Run(...)` / `handle.Resume(...)`
- **`Registry` 是粗粒度锁的会话注册表**（handle.go:111-117）注释：
  > "并发安全（细粒度乐观并发由业务层负责，本层以粗粒度锁保证一致性）"
- **`bus` 可选**：有 bus 就广播 Agent 域事件，无 bus 就当 no-op

---

## 3. `NewAgent` 装配回调（handle.go:22-24）

```go
type NewAgent func(sess *session.Session) (*driver.Agent, *driver.RunContext, error)
```

**为什么是回调而不是直接传配置？**

注释（handle.go:23-24）：

> "Registry 依赖它完成会话级装配（**由 assistant 提供**，复用其驱动工厂）"

也就是说：handle 包不关心"怎么构造 Agent"，只关心"调 NewAgent 拿个 Agent 出来"。具体的驱动选择、钩子注册、capability 注入由 `assistant` 包封装好后传进来。

**典型实现**（来自 assistant 包）：

```go
reg := handle.NewRegistry(
    func(sess *session.Session) (*driver.Agent, *driver.RunContext, error) {
        rt := buildRunContext(assistantCfg)
        rt.Session = sess
        return driver.New(rt, react.NewReAct(rt)), rt, nil
    },
    checkpointStore,
    eventBus,  // 可选
)
```

---

## 4. `AgentHandle`：单会话句柄（handle.go:30-40）

```go
type AgentHandle struct {
    ID      string
    Session *session.Session
    Agent   *driver.Agent
    RT      *driver.RunContext
    Scope   *scope.Scope
    Store   checkpoint.Store

    disposed bool
}
```

注释（handle.go:27-30）解释三个动作：

> "Create 后可用 Run / RunStream 执行；挂起（SuspendError）后经 Resume(decision) 从 checkpoint 续跑；Dispose 释放（Scope 逆序回滚 + checkpoint 清理）"

### 4.1 `Run` / `RunStream`：执行一次任务（handle.go:43-48）

```go
func (h *AgentHandle) Run(ctx context.Context, input string) (string, error) {
    if h == nil || h.Agent == nil {
        return "", fmt.Errorf("agent: handle: nil agent")
    }
    return h.Agent.Run(ctx, h.Session, input)
}
```

**薄封装**：直接调 `Agent.Run`。注释（react.md 同主题）解释 `Agent.RunStream` 的入口也在 driver 层。

### 4.2 `Resume` 从挂起点续跑（handle.go:57-90）

这是 M2.8 异步审批的核心续跑入口。流程图：

```
Resume(ctx, decision)
   │
   ├─ h.Store.Load(ctx, sessionID)
   │     ├─ checkpoint 不存在 → 普通续跑路径
   │     └─ checkpoint 存在 → 注入决策 + 续跑
   │
   ├─ ckpt.Decisions[ckpt.SuspendedCallID] = d  (用户决策)
   ├─ ckpt.ResolvedAt = &now
   ├─ h.Store.Save(ctx, ckpt)                    (持久化决策)
   ├─ h.RT.ResumeCheckpoint = ckpt                (注入 RunContext)
   └─ _ = h.Session.Append(SessionEventApprovalResolve, ...)
   │
   ├─ h.Agent.Run(ctx, h.Session, "")
   │     └─ driver.runResumeTurn 直接进入工具批量（policyGuard 按 Decisions 查表放行）
   │
   ├─ 任务完成 → h.Store.Delete(ctx, sessionID)   (清理 checkpoint)
   └─ return reply, nil
```

**几个值得品的细节**：

1. **决策注入用 map 而不是直接覆盖字段**（handle.go:67）：
   > "ckpt.Decisions = map[string]ApprovalDecision{ckpt.SuspendedCallID: d}"

   注释（approval.md §2）提到一个 ID 可能有多个 call 同时挂起，但 SuspendedCallID 是单值 —— 这条 Resume 只针对单个挂起 call。Decisions map 留有扩展空间。

2. **`ResolvedAt` 时间戳**（handle.go:68-69）—— 标记决策时刻，审计用

3. **Save 后再注入**（handle.go:70-73）—— 先持久化决策，再让 RunContext 引用，避免驱动读到的 checkpoint 没持久化

4. **resume 路径调 `h.Agent.Run(ctx, h.Session, "")`**（handle.go:81）—— input 为空字符串，因为 resume 不需要新用户输入；driver 内部靠 `rt.ResumeCheckpoint != nil` 判断走 `runResumeTurn` 而非 `runTurnInner`

5. **任务完成后清理 checkpoint**（handle.go:86-88）—— 注释："任务完成：清理挂起点（checkpoint + 任务状态标记完成）"

### 4.3 `Dispose` 幂等释放（handle.go:93-106）

```go
func (h *AgentHandle) Dispose(ctx context.Context) error {
    if h == nil || h.disposed {
        return nil        // 幂等保护
    }
    h.disposed = true
    if h.Scope != nil {
        h.Scope.Close()    // Scope 逆序回滚（详见 scope 包）
    }
    h.RT.Notify(agent.Event{Type: agent.EventDisposed, SessionID: h.ID})
    _ = h.Session.Append(event.SessionEventAgentDisposed, event.AgentHandleData{...})
    return nil
}
```

**两条不变量**：

1. **幂等** —— `disposed` 标志保护，多次 Dispose 安全
2. **Scope 逆序回滚** —— 资源释放顺序与注册顺序相反（LIFO）

---

## 5. `Registry`：会话注册表（handle.go:111-127）

```go
type Registry struct {
    mu       sync.Mutex
    handles  map[string]*AgentHandle
    newAgent NewAgent
    store    checkpoint.Store
    bus      *event.EventBus
}

func NewRegistry(newAgent NewAgent, store checkpoint.Store, bus *event.EventBus) *Registry {
    return &Registry{
        handles:  make(map[string]*AgentHandle),
        newAgent: newAgent,
        store:    store,
        bus:      bus,
    }
}
```

### 5.1 `Create` 新建会话（handle.go:130-148）

```go
func (r *Registry) Create(ctx context.Context, sess *session.Session) (*AgentHandle, error) {
    ag, rt, err := r.newAgent(sess)
    if err != nil { return nil, err }
    h := &AgentHandle{
        ID: sess.ID, Session: sess, Agent: ag, RT: rt,
        Scope: scope.New(), Store: r.store,
    }
    r.mu.Lock()
    r.handles[sess.ID] = h
    r.mu.Unlock()
    r.emit(ctx, event.SessionEventAgentCreated, event.AgentHandleData{...})
    return h, nil
}
```

**三步走**：

1. `r.newAgent(sess)` —— 调回调拿 Agent + RT（构造失败直接返回）
2. 构造 AgentHandle 并塞进 map
3. 发 `agent/created` 事件

### 5.2 `Resume` 从持久化恢复（handle.go:151-186）

与 `AgentHandle.Resume` 不同 —— 这是**Registry 层**的 Resume，**没有指定 ApprovalDecision**：

```go
func (r *Registry) Resume(ctx context.Context, sess *session.Session) (*AgentHandle, error) {
    // 1. 加载 checkpoint
    ckpt, err := r.store.Load(ctx, sess.ID)
    if err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
        return nil, fmt.Errorf(...)
    }
    
    // 2. 移除 RepairInterrupted 为挂起轮合成的 turn/end{interrupted}
    if ckpt != nil {
        session.DropInterruptedForTurn(sess, ckpt.Turn)
    }
    
    // 3. 构造 Agent
    ag, rt, err := r.newAgent(sess)
    if err != nil { return nil, err }
    
    // 4. 资源校验（详见 missing.go）
    if err := ValidateMissing(rt, sess); err != nil {
        return nil, err
    }
    
    // 5. 挂 checkpoint
    if ckpt != nil {
        rt.ResumeCheckpoint = ckpt
    }
    
    // 6. 构造句柄 + 入表 + 发事件
    h := &AgentHandle{...}
    r.mu.Lock()
    r.handles[sess.ID] = h
    r.mu.Unlock()
    r.emit(ctx, event.SessionEventAgentStatus, event.AgentHandleData{..., Status: "resumed"})
    return h, nil
}
```

**关键设计**：

1. **`DropInterruptedForTurn` 移除合成 interrupted**（handle.go:159）—— 这是 session.md §4.5 提到的反操作。RepairInterrupted 会为孤儿 turn/start 合成 turn/end{interrupted}，但**刻意挂起**的轮次不应有这个 end（会在续跑成功后补写真正的 end）。所以 Resume 时移除。
2. **`ValidateMissing` 资源校验**（handle.go:167）—— 见 §6
3. **没有决策注入** —— Registry.Resume 是"加载到内存"，决策注入在 `AgentHandle.Resume` 里做（因为那时用户已经做了决策）

### 5.3 `Get` 查句柄（handle.go:189-194）

```go
func (r *Registry) Get(id string) (*AgentHandle, bool) {
    r.mu.Lock()
    defer r.mu.Unlock()
    h, ok := r.handles[id]
    return h, ok
}
```

简单 map 查询，锁内完成。

### 5.4 `DisposeAll` 全量释放（handle.go:197-211）

```go
func (r *Registry) DisposeAll(ctx context.Context) error {
    r.mu.Lock()
    hs := make([]*AgentHandle, 0, len(r.handles))
    for _, h := range r.handles {
        hs = append(hs, h)
    }
    r.mu.Unlock()
    var firstErr error
    for _, h := range hs {
        if err := h.Dispose(ctx); err != nil && firstErr == nil {
            firstErr = err
        }
    }
    return firstErr
}
```

**几个细节**：

1. **拷贝 handles 后释放锁** —— 防止 Dispose 持有锁过久
2. **聚合首个错误** —— 用 `firstErr` 模式而不是 errors.Join（与 eventbus.go 不同，因为这里是"清理"路径，第一个错误通常最有意义）

### 5.5 `emit` 广播（handle.go:214-219）

```go
func (r *Registry) emit(ctx context.Context, name string, payload any) {
    if r.bus == nil { return }
    _ = r.bus.Emit(ctx, name, payload)
}
```

bus 可选 —— 这是装配友好的设计，老代码 / 测试可以不传 bus。

---

## 6. 资源校验：`ValidateMissing`（missing.go:29-66）

### 6.1 问题域

注释（missing.go:12-14）：

> "对齐 PI SuspendedOperation 的 missing 校验：resume 前校验会话事件引用的工具在 RunContext.Tools 中存在，缺失则明确报错，避免'恢复后跑一半发现工具没了'的静默失败"

也就是说：**如果平台换了工具集再 resume**，旧的 tool/call 事件引用的工具在当前 RT 中不存在，Resume 应该直接报错，而不是跑到一半才报错。

### 6.2 实现

```go
func ValidateMissing(rt *driver.RunContext, sess *session.Session) error {
    if rt == nil || sess == nil { return nil }
    seen := make(map[string]bool)
    var missing []string
    for _, ev := range sess.Events {
        if ev.Type != event.SessionEventToolCall && ev.Type != event.SessionEventToolResult {
            continue
        }
        var name string
        switch ev.Type {
        case event.SessionEventToolCall:
            var d event.ToolCallData
            json.Unmarshal(ev.Data, &d)
            name = d.Name
        case event.SessionEventToolResult:
            var d event.ToolResultData
            json.Unmarshal(ev.Data, &d)
            name = d.Name
        }
        if name == "" || seen[name] { continue }
        seen[name] = true
        if _, ok := rt.Tools.Get(name); !ok {
            missing = append(missing, name)
        }
    }
    if len(missing) > 0 {
        return Missing{Tools: missing}
    }
    return nil
}
```

**几个值得品的设计**：

1. **seen map 去重** —— 同一个工具被多次调用只报告一次
2. **同时扫 tool/call 和 tool/result** —— 保证"调用过"和"结果过"都能被覆盖
3. **Planner 缺失不在此校验**（missing.go:28 注释）：
   > "模型引用（rt.Planner）缺失不在此校验（Planner 由装配层保证非 nil）"
   
   即：工具的 missing 是业务配置变更，模型引用是装配错误，两者语义不同

### 6.3 `Missing` 错误类型

```go
type Missing struct {
    Tools []string
}

func (m Missing) Error() string {
    return fmt.Sprintf("agent: resume missing resources: tools %v not in RunContext", m.Tools)
}

func (m Missing) HasMissing() bool { return len(m.Tools) > 0 }
```

- 实现 `error` 接口
- `HasMissing()` 让调用方能用 `errors.As` + 类型断言拿到工具名列表

---

## 7. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **AgentHandle 与会话一一对应** | `ID = Session.ID` | 一会话一句柄 |
| **Dispose 幂等** | `disposed` 标志 | 多次调用安全 |
| **Scope 逆序回滚** | `h.Scope.Close()` | LIFO 资源释放 |
| **粗粒度锁一致性** | Registry.mu | 不假设业务并发模型 |
| **Decisions 注入 SuspendedCallID 键** | handle.go:67 | 单决策对应单挂起 call |
| **Save checkpoint 后再注入 RT** | handle.go:70-73 | 持久化先于引用 |
| **checkpoint 删除 = 任务完成** | handle.go:86-88 | 防止重 Resume 误用 |
| **Resume 前 DropInterruptedForTurn** | handle.go:159 | 移除合成 end，留真实 end |
| **资源校验失败明确报错** | ValidateMissing | 防止静默失败 |
| **Planner 缺失不在 missing 校验范围** | missing.go:28 | 业务/装配错误分类 |
| **bus 可选** | emit `b == nil` 跳过 | 装配友好 |

---

## 8. 与其它模块的协作边界

```
┌─────────────────────────────────────────────────────────────┐
│                        Platform                              │
│   （Web / CLI / Inbox / 异步审批流）                          │
└────────────────┬────────────────────────────────────────────┘
                 │
                 ▼
┌─────────────────────────────────────────────────────────────┐
│              assistant.New(...) / handle.Registry            │
│   （业务侧装配：NewAgent 回调 + checkpoint.Store + bus）      │
└────────────────┬────────────────────────────────────────────┘
                 │
                 ▼
┌─────────────────────────────────────────────────────────────┐
│           AgentHandle (会话级句柄)                            │
│   Create / Run / Resume / Dispose                           │
└──┬──────────┬─────────┬─────────┬──────────┬────────────────┘
   │          │         │         │          │
   ▼          ▼         ▼         ▼          ▼
┌──────┐ ┌────────┐ ┌────────┐ ┌───────┐ ┌──────────┐
│session│ │ driver │ │  scope │ │checkpt│ │  event   │
│      │ │  Agent │ │        │ │ Store │ │   Bus    │
└──────┘ └────────┘ └────────┘ └───────┘ └──────────┘
```

handle 包是"上层粘合层"，依赖面广但不复杂：

- 依赖 `driver`（驱动 Agent + RT）
- 依赖 `session`（会话状态）
- 依赖 `scope`（资源作用域）
- 依赖 `checkpoint`（挂起点存储）
- 依赖 `event`（事件类型 + Bus）
- 依赖 `approval`（决策注入）

**handle 包不依赖** `tool` / `llm` / `planner` —— 这些是装配层的细节，通过 `NewAgent` 回调传进来。

---

## 9. 自定义扩展姿势

### 9.1 平台侧：Web 后台集成

```go
// 用户发请求 → 后台创建会话
sess := session.NewSession(req.SessionID)
h, err := reg.Create(ctx, sess)
if err != nil { return err }

go func() {
    reply, err := h.Run(ctx, req.Input)
    if err != nil {
        var suspend *approval.SuspendError
        if errors.As(err, &suspend) {
            // 通知 Web 前端弹审批
            webUI.ShowApproval(suspend.ID, suspend.Request)
            return
        }
        // 其他错误
    }
    webUI.ShowResult(reply)
}()
```

### 9.2 平台侧：用户决策后 Resume

```go
// Web 前端收到用户决策后
func onUserDecision(sessionID string, approved bool, reason string) {
    h, ok := reg.Get(sessionID)
    if !ok { return }
    
    reply, err := h.Resume(ctx, approval.ApprovalDecision{
        Approved: approved, Reason: reason, DecidedBy: "user@example.com",
    })
    // 二次挂起 → 通知前端 → 等等
    // 正常完成 → 显示结果
}
```

### 9.3 平台侧：应用退出清理

```go
defer reg.DisposeAll(ctx)
```

### 9.4 平台侧：自定义资源校验

```go
// missing.go 只校验工具；如果你想加 Planner / 模型校验：
type PlannerMissing struct{ Models []string }

func ValidatePlannerMissing(rt *driver.RunContext, sess *session.Session) error {
    // 你的实现
}

// 在 Registry.Resume 里加一行
if err := ValidatePlannerMissing(rt, sess); err != nil { return err }
```

但要注意注释（missing.go:28）的提示："Planner 由装配层保证非 nil" —— 平台应保证装配时不传 nil。

### 9.5 用 Session.Persistence 替代 Store

handle 只用了 `checkpoint.Store`，但 session 自身的持久化（`Persistence`）通过 `rt.Session.Persistence` 单独走。两者**职责分离**：

| 存储 | 用途 |
|---|---|
| `checkpoint.Store` | 挂起点（含 Decisions、Snapshot） |
| `session.Persistence` | 事件日志 + 任务状态 |

---

## 10. 与 Approval / Checkpoint 的协作总结

```
┌────────────────────────────────────────────────────────────┐
│             异步审批 + Checkpoint 完整链路                    │
└────────────────────────────────────────────────────────────┘

1. Driver.policyGuard 返回 *approval.SuspendError
   ↓
2. Driver.runResumeTurn/saveCheckpoint:
   - 写 approval/request 事件
   - Checkpoint.Save: Calls + Request + Decisions + Snapshot
   - Persistence.SaveState: TaskState（Facts 等）
   ↓
3. Driver.Run 返回 SuspendError → 退出
   ↓
4. 平台侧：用户决策 → Approval.Resolve(id, decision)
   ↓
5. 平台侧调 Registry.Get(id) 拿句柄 → AgentHandle.Resume(ctx, decision)
   - Store.Load → Decisions[SuspendedCallID] = decision
   - Store.Save (持久化决策)
   - 写 approval/resolve 事件
   - Agent.Run 触发 runResumeTurn
   ↓
6. Driver.runResumeTurn 直接进入工具批量
   - policyGuard 按 Decisions[call.ID] 查表放行
   - 已决策的调用正常执行
   ↓
7. 任务完成：
   - 调 AgentHandle.Resume 的尾部 Store.Delete（清理 checkpoint）
   - 调 Registry.Create/Resume 时会发 agent/created / agent/status(resumed) 事件
   ↓
9. 平台侧调 Handle.Dispose：
   - Scope 逆序回滚
   - 写 agent/disposed 事件
```

handle 是这条链路**唯一对外的入口**，让平台用简洁的 API 管理整个生命周期。

---

## 11. 一句话总结

**`agent/handle` 是 MoS Agent 的"生命周期管理层"：用 `NewAgent` 回调把"怎么构造 Agent"完全交给上层（assistant），用 `AgentHandle` 把"会话 + Agent + RunContext + Scope + Store"绑成会话级不变量，用 `Registry.Create/Resume/Dispose/DisposeAll` 提供"像管理 HTTP 连接一样管理智能体"的 API，用 `AgentHandle.Resume` 把 M2.8 异步审批续跑（checkpoint 加载 → 决策注入 → 跳过 LLM → 工具批量执行 → 清理 checkpoint）的完整流程压缩成一行调用，用 `ValidateMissing` 把"换工具集后再 resume"的静默失败转成明确报错。** 它是整个 agent 子系统面向平台的最外圈，所有异步审批、跨进程恢复、长任务管理都从这里进入。