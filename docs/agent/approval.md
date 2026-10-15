# 异步审批通道（agent/approval）

> 来源：`agent/approval/approval.go`（接口 + 错误类型 + 上下文辅助） + `inmem.go`（InMemApproval 内存实现） + `confirmer_adapter.go`（Confirmer 兼容层）。
> 目标读者：理解"工具调用需要人工确认时发生了什么"的所有人；想实现自定义审批后端（Web / Redis / 数据库）的开发者。

---

## 1. 一句话定位

**`agent/approval` 是 agent 的"异步审批通道"**：把"工具调用前需要人工确认"这件事抽象成接口（`Approval`），让 driver 在守卫内发起请求（`Request`）后立刻挂起（`SuspendError` 退出 Run），平台异步 `Resolve` 后经 `Handle.Resume` 续跑。**one-shot 由实现强制，未配置 Approval 时对需要确认的操作 fail-closed 拒绝**。

文件分布：

| 文件 | 职责 |
|---|---|
| `approval.go` | `Approval` 接口、`Immediate` 可选接口、`ApprovalRequest`/`ApprovalDecision`、`SuspendError`、`RequiredError`、ctx 辅助 |
| `inmem.go` | `InMemApproval` 默认内存实现（单进程、并发安全） |
| `confirmer_adapter.go` | `ConfirmerAdapter`：把旧同步 `Confirmer` 适配成 `Approval`，保留 CLI y/N 体验 |

---

## 2. 整体协作流（架构 v2 §3.4 + §10.6）

这是整个 M2.8 异步审批最关键的流程图：

```
驱动 policyGuard
  │
  ├─ Policy.Allow(ctx, call)
  │     │
  │     ├─ nil → 放行
  │     │
  │     ├─ 硬错误（非 RequiredError）→ 直接拒绝
  │     │
  │     └─ *RequiredError → 需人工确认
  │           │
  │           ├─ rt.Approval == nil → fail-closed 拒绝
  │           │
  │           ├─ 实现 Immediate 且 Decision 立即可用 → 放行/拒绝（不挂起）
  │           │
  │           └─ 否则 → Approval.Request(ctx, req) → ID
  │                 │
  │                 └─ return *SuspendError{ID, Request, CallID, HookKind, ReplayTurn, ReplayStep}
  │
  ▼
driver.runResumeTurn / saveCheckpoint
  │
  ├─ 写 approval/request 事件（durable 审计）
  ├─ 写 checkpoint（含挂起轮工具批 + Decisions map + Snapshot）
  ├─ 任务状态 SaveState 落盘
  │
  ▼
driver 冒泡 SuspendError → 退出 Run
  │
  ▼
平台异步：用户决策后调 Approval.Resolve(id, decision)
  │
  ▼
Handle.Resume(decisions) → 重建 Agent + 设置 rt.ResumeCheckpoint
  │
  ▼
Driver.Run → runResumeTurn 跳过 LLM，按 Decisions[call.ID] 查表放行
```

---

## 3. 核心接口：`Approval` 与 `Immediate`

### 3.1 `Approval`（approval.go:47-50）

```go
type Approval interface {
    Request(ctx context.Context, req ApprovalRequest) (ApprovalID, error)
    Resolve(ctx context.Context, id ApprovalID, d ApprovalDecision) error
}
```

注释（approval.go:41-46）写得很直白：

> "Request 发起审批，返回 ID；**不阻塞 driver**，driver 拿到 ID 后抛 SuspendError 退出 Run。Resolve 由平台在收到用户决定后调用；与 Request 配对，**一次性（one-shot）**"

**两条不变量**：
1. **Request 不阻塞** —— 拿到 ID 就走，决策是后续 Resolve 的事
2. **one-shot** —— 同一个 ID 二次 Resolve 必须报 `ErrAlreadyResolved`（inmem.go:48）

### 3.2 `Immediate`（approval.go:54-56）

```go
type Immediate interface {
    Decision(ctx context.Context, id ApprovalID) (ApprovalDecision, bool)
}
```

注释（approval.go:52-53）：

> "驱动识别到该接口且决策立即可用时直接应用决策、不挂起——保留旧 CLI y/N 同步体验"

**接口隔离原则**：是否同步不是 `Approval` 的事，是 `Immediate` 这条**可选接口**的事。`InMemApproval` 不实现（真的异步），`ConfirmerAdapter` 实现（同步确认）。

---

## 4. 关键数据结构

### 4.1 `ApprovalID`：字符串 UUID

```go
type ApprovalID string
```

为什么用 string 而不是 int/uint64：
- 跨进程友好（持久化到 checkpoint.Store / 数据库时是字符串）
- UUID 防冲突（inmem.go:36 用 `uuid.NewString()` 生成）
- 平台 Resolve 时直接回传，类型安全

### 4.2 `ApprovalRequest`（approval.go:22-27）

```go
type ApprovalRequest struct {
    ToolName string `json:"tool_name"`
    Command  string `json:"command"`
    Reason   string `json:"reason"`
    Args     string `json:"args,omitempty"`
}
```

注释（approval.go:21）：

> "Args 是工具调用参数 JSON 原文，供平台展示上下文；不参与驱动逻辑"

也就是说：**Args 是给人看的**，驱动逻辑只看 ToolName/Command/Reason。平台 UI 可以把整个 Args 渲染出来让人看上下文。

### 4.3 `ApprovalDecision`（approval.go:33-39）

```go
type ApprovalDecision struct {
    Approved  bool      `json:"approved"`
    Reason    string    `json:"reason,omitempty"`
    Content   string    `json:"content,omitempty"`    // request_human_input 的回答
    DecidedBy string    `json:"decided_by,omitempty"`
    DecidedAt time.Time `json:"decided_at"`
}
```

**`Content` 字段的特殊用途**（approval.go:31-32）：

> "Content 承载人类自由文本回答（request_human_input 工具：回答即工具结果）；普通确认（放行/拒绝）仅用 Approved/Reason"

这是 M2.8 之后扩展出来的能力 —— **不只是 y/N，还能承载人类主动输入的回答**，比如 `request_human_input` 工具问"用哪个数据库？"，人类回"MySQL"，Content 就是"MySQL"，工具直接拿到这个 Content 作为结果。

### 4.4 `SuspendError`：driver 抛出的挂起信号（approval.go:109-123）

```go
type SuspendError struct {
    ID         ApprovalID
    Request    ApprovalRequest
    CallID     string    // 挂起的工具调用 ID
    HookKind   hook.HookKind
    ReplayTurn int
    ReplayStep int
}
```

**关键字段解释**：

| 字段 | 作用 |
|---|---|
| `ID` | 关联平台 Resolve 调用 |
| `Request` | 给平台展示用，冗余存储以便离线审计 |
| `CallID` | checkpoint 恢复时按此键控 Decisions map |
| `HookKind` | 标记挂起发生的钩子点（如 tool/pre-execute） |
| `ReplayTurn/Step` | Resume 时直接跳到这个位置 |

注释（approval.go:107-108）：

> "驱动层在批量守卫预检返回后，经 errors.As 检测即写 checkpoint + 冒泡退出 Run。**叶子错误，不实现 Unwrap**"

**`不实现 Unwrap` 是有意为之**：SuspendError 不是某个底层错误的包装，而是 driver 主动抛出的**控制流信号**。如果它 Unwrap 到底层错误，调用方可能会误以为"出了什么错"，实际上这是"主动暂停等审批"。

### 4.5 `RequiredError`：策略侧的"需人工确认"标记（approval.go:61-68）

```go
type RequiredError struct {
    Request ApprovalRequest
}

func (e *RequiredError) Error() string {
    return "approval required for tool " + e.Request.ToolName
}
```

注释（approval.go:58-60）：

> "区别于普通策略错误（路径越界等硬拒绝，直接拦截回填）"

也就是说 `RequiredError` 是**软标记** —— 不是"做错了"，而是"需要确认"。driver 用 `errors.As` 识别后走 approval 路径，不会当硬错误处理。

---

## 5. 上下文辅助（approval.go:78-103）

两个 ctx key：

```go
func WithApproval(ctx, a Approval) context.Context
func ApprovalFrom(ctx) (Approval, bool)

func WithResumeDecisions(ctx, decisions map[string]ApprovalDecision) context.Context
func ResumeDecisions(ctx) map[string]ApprovalDecision
```

**用法场景**：
- `WithApproval`：**驱动在工具执行前注入**，让工具能直接 `ApprovalFrom(ctx)` 拿审批通道发起请求（比如 `request_human_input` 工具就是这种用法）
- `WithResumeDecisions`：**Resume 时注入**，让工具在 Resume 路径能从 ctx 读到人类回答（`request_human_input` 的 Content 字段）

这两个 helper 是**反向依赖解耦**的典范 —— 工具不直接依赖 `RunContext`，通过 ctx 拿需要的对象。

---

## 6. 错误类型与幂等保证

```go
var ErrUnknownID       = errors.New("approval: unknown id")
var ErrAlreadyResolved = errors.New("approval: already resolved")
```

注释（approval.go:70-71）：

> "ErrUnknownID 标识 Resolve 未知 ID；Resolve 应被**幂等处理**（不 panic）"

**为什么幂等？** 平台可能因为网络重试多次 Resolve，未知 ID / 已 Resolve 都应该是**软错误**，让平台能 retry 或 skip，而不是让 driver panic。

---

## 7. `InMemApproval`：默认内存实现

### 7.1 数据结构（inmem.go:19-23）

```go
type InMemApproval struct {
    mu       sync.Mutex
    pending  map[ApprovalID]chan ApprovalDecision  // 待决
    resolved map[ApprovalID]ApprovalDecision       // 已决
}
```

**双 map 设计**：
- `pending`：Request 时预占 chan（容量 1），Resolve 时写入
- `resolved`：Resolve 后持久化（防止 chan 关闭后再查找不到）

### 7.2 关键方法

#### `Request`（inmem.go:35-41）

```go
func (a *InMemApproval) Request(ctx context.Context, req ApprovalRequest) (ApprovalID, error) {
    id := ApprovalID(uuid.NewString())
    a.mu.Lock()
    a.pending[id] = make(chan ApprovalDecision, 1)  // 容量 1
    a.mu.Unlock()
    return id, nil
}
```

**注意**：`Request` 不阻塞，不调 `inner.Confirm`，**真的等异步 Resolve**。这与 `ConfirmerAdapter.Request` 形成对比（见 §8）。

#### `Resolve`（inmem.go:45-63）

```go
func (a *InMemApproval) Resolve(ctx context.Context, id ApprovalID, d ApprovalDecision) error {
    a.mu.Lock()
    if _, done := a.resolved[id]; done {
        a.mu.Unlock()
        return ErrAlreadyResolved   // one-shot 保护
    }
    ch, ok := a.pending[id]
    if !ok {
        a.mu.Unlock()
        return ErrUnknownID
    }
    delete(a.pending, id)
    a.resolved[id] = d
    a.mu.Unlock()
    
    ch <- d
    close(ch)
    return nil
}
```

**几个细节**：

1. **先改 resolved 再 unlock，最后写 chan** —— 保证并发场景下"已 resolved" 的状态可见性先于 chan 通知
2. **`close(ch)` 在写之后** —— 让 `Await` 的 `case d, ok := <-ch` 能通过 `ok == false` 检测到关闭
3. **`ch` 容量 1** —— Resolve 写一次后必然能写入（不会阻塞），但二次 Resolve 走 ErrAlreadyResolved 路径根本不会到这里

#### `Await`（inmem.go:67-87）

```go
func (a *InMemApproval) Await(ctx context.Context, id ApprovalID) (ApprovalDecision, error) {
    a.mu.Lock()
    if d, done := a.resolved[id]; done {
        a.mu.Unlock()
        return d, nil                // 快速路径：已决议
    }
    ch, ok := a.pending[id]
    a.mu.Unlock()
    if !ok {
        return ApprovalDecision{}, ErrUnknownID
    }
    select {
    case d, ok := <-ch:
        if !ok {
            return ApprovalDecision{}, ErrUnknownID  // chan 关闭
        }
        return d, nil
    case <-ctx.Done():
        return ApprovalDecision{}, ctx.Err()         // ctx 取消
    }
}
```

注释（inmem.go:65-66）：

> "Await 读取指定 ID 的决策：已 Resolve 立即返回；未 Resolve 阻塞等待（ctx 可取消）。**测试 / 本地平台注入决策用**"

也就是说 **`Await` 不是 driver 用的**，driver 走 SuspendError 冒泡路径；Await 是给测试 / 本地 CLI 平台同步等待决策用的。

#### `Pending`（inmem.go:89-97）

观测用，单测断言当前未决数量。`a == nil` 安全（nil receiver 返 0）。

### 7.3 为什么不实现 `Immediate`

注释（inmem.go:18）明文：

> "不实现 Immediate —— Request 后驱动总是挂起，决策经 Resolve → Handle.Resume 注入续跑"

这是有意设计：**真异步就该真挂起**，否则测试时容易误以为"调了 Request 就拿到决策"。

---

## 8. `ConfirmerAdapter`：旧同步 Confirmer 的兼容层

### 8.1 为什么需要兼容层

注释（confirmer_adapter.go:14-16）：

> "Confirmer 是旧的同步确认接口（M2.8 前，assistant 包）。保留为兼容层，通过 ConfirmerAdapter 桥接为异步 Approval。新代码应直接实现 Approval"

也就是说：**M2.8 之前**，确认是同步的 `Confirm(ctx, req) (bool, error)`，CLI 一问一答；**M2.8 之后**改成了异步 Approval，但要保留旧 CLI 体验。

### 8.2 工作原理

```go
func (a *ConfirmerAdapter) Request(ctx, req) (ApprovalID, error) {
    ok, err := a.inner.Confirm(ctx, ConfirmRequest{...})  // 同步调，确认完再返回
    ...
    id := ApprovalID(uuid.NewString())
    a.table[id] = ApprovalDecision{Approved: ok, DecidedAt: time.Now()}
    return id, nil
}
```

**关键点**：
- `Request` 内部**同步**调 `inner.Confirm` 拿到决策
- 决策存入内部 table（`a.table`）
- driver 看到实现了 `Immediate`，调 `Decision` 拿决策，**不挂起**

### 8.3 与 `InMemApproval` 的对比

| 维度 | `InMemApproval` | `ConfirmerAdapter` |
|---|---|---|
| Request 行为 | 立即返回，不阻塞 | 同步调 Confirm，阻塞到用户决策 |
| 决策存储 | chan + resolved map | 内部 table |
| Immediate 实现 | ❌ 不实现 | ✅ 实现 |
| driver 行为 | 总是挂起 | 直接放行/拒绝 |
| 适用场景 | 真异步（Web/Redis） | 旧 CLI y/N |
| Resolve 调用 | 必需 | no-op（Request 时已决） |

注释（confirmer_adapter.go:33-35）：

> "ConfirmerAdapter 只适用于'确认本身是快速同步调用'的场景（CLI y/N）；真正的异步人工介入（Web 弹窗 / 审批流）应直接实现 Approval"

### 8.4 `Resolve` no-op 设计

```go
func (a *ConfirmerAdapter) Resolve(ctx, id, d) error {
    return nil
}
```

注释（confirmer_adapter.go:80-81）：

> "ConfirmerAdapter 在 Request 时已同步确认，无异步回填"

这是接口兼容性的妥协 —— `Approval` 接口必须实现 `Resolve`，但 ConfirmerAdapter 走同步路径，Resolve 永远不会被调用，返回 nil 让接口契约完整。

### 8.5 `ConfirmRequest`：避免 import cycle

```go
type ConfirmRequest struct {
    ToolName string
    Command  string
    Reason   string
}
```

注释（confirmer_adapter.go:21-22）：

> "与 assistant.ConfirmRequest 同构，避免 import cycle：assistant 会 import approval，approval 不得回引 assistant"

这是**包依赖单向性**的经典处理 —— approval 不能反向依赖 assistant，所以在 approval 包里复制一份同构的 ConfirmRequest。

---

## 9. Confirmer vs Approval 的语义对照

| 维度 | 旧 Confirmer | 新 Approval |
|---|---|---|
| 调用时机 | 同步：driver 等用户回答 | 异步：driver 立即挂起，平台 Resolve |
| 用户等待 | 阻塞 | 不阻塞（driver 已退出） |
| Resume 路径 | 无（一次性） | 必需（Handle.Resume） |
| 失败处理 | error 返回 | SuspendError 冒泡 |
| 持久化 | 不需要 | checkpoint 必需 |
| Content 字段 | 无（仅 bool） | 有（承载人类回答） |

**M2.8 的核心升级**：从"同步阻塞式确认"到"异步 Suspend + Resume"，让长任务 / Web 弹窗 / 审批流 / Inbox 模式成为可能。

---

## 10. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **Request 不阻塞** | `InMemApproval.Request` 立即返回 | driver 拿到 ID 后即可挂起 |
| **one-shot 由实现强制** | `Resolve` 检查 resolved map | 二次 Resolve 报 ErrAlreadyResolved |
| **幂等处理 Resolve** | `ErrUnknownID` / `ErrAlreadyResolved` 不 panic | 平台 retry / skip 安全 |
| **SuspendError 叶子错误** | 不实现 Unwrap | driver 用 errors.As 识别，不当硬错误 |
| **RequiredError 是软标记** | `errors.As` 区分 | 与硬错误（路径越界）分开走不同路径 |
| **未配 Approval 时 fail-closed** | react.go:472-474 显式检查 | 缺能力不裸奔 |
| **ctx 注入避免反向依赖** | `WithApproval` / `WithResumeDecisions` | 工具不依赖 RunContext |
| **InMem 不实现 Immediate** | 接口隔离原则 | 真异步应该真挂起 |
| **ConfirmerAdapter 适配同步体验** | 实现 Immediate | 旧 CLI 体验保留 |
| **`Content` 承载人类回答** | `ApprovalDecision.Content` | request_human_input 工具用 |

---

## 11. 自定义扩展姿势

### 11.1 实现 Web 后端

```go
type WebApproval struct {
    apiURL string
    http   *http.Client
}

func (w *WebApproval) Request(ctx context.Context, req ApprovalRequest) (ApprovalID, error) {
    resp, err := w.http.Post(w.apiURL+"/approval", "application/json", ...)
    // 返回平台侧分配的 ID（也可本地 UUID + 平台侧绑定）
    return ApprovalID(id), nil
}

func (w *WebApproval) Resolve(ctx context.Context, id ApprovalID, d ApprovalDecision) error {
    // 平台侧异步 Resolve 后调这里
    // 也可不实现（平台直接驱动 Handle.Resume）
    return nil
}
```

### 11.2 实现 Redis 后端（持久化 Resume）

```go
type RedisApproval struct {
    rdb *redis.Client
}

func (r *RedisApproval) Request(ctx context.Context, req ApprovalRequest) (ApprovalID, error) {
    id := ApprovalID(uuid.NewString())
    r.rdb.Set(ctx, "approval:"+string(id), "pending", 24*time.Hour)
    // 发布到平台通知队列
    return id, nil
}

func (r *RedisApproval) Resolve(ctx context.Context, id ApprovalID, d ApprovalDecision) error {
    key := "approval:" + string(id)
    if exists := r.rdb.Exists(ctx, key); !exists {
        return ErrUnknownID
    }
    data, _ := json.Marshal(d)
    r.rdb.Set(ctx, key, data, 24*time.Hour)
    // 通知等待者（pub/sub）
    return nil
}
```

这样重启后 pending 决策依然能恢复（关键是 checkpoint 也得持久化到 Redis 或数据库）。

### 11.3 平台侧 Resolve 触发 Handle.Resume

```go
// 平台 Web 收到用户决策后：
func handleUserDecision(id ApprovalID, approved bool) {
    approval.Resolve(ctx, id, ApprovalDecision{Approved: approved, DecidedAt: time.Now()})
    // 然后查找挂起该 ID 的 AgentHandle，调 handle.Resume(decisions)
}
```

`Handle.Resume` 的具体契约见 `docs/agent/handle.md`（后续包）。

### 11.4 用 ConfirmerAdapter 保留 CLI 体验

```go
confirmer := myOldConfirmer{}  // 旧 CLI y/N 实现
rt.Approval = approval.NewConfirmerAdapter(confirmer)
// 驱动识别到 Immediate，Request 同步确认后立即放行/拒绝，不挂起
```

### 11.5 工具内发起审批（`request_human_input`）

```go
// request_human_input 工具内部
a, ok := approval.ApprovalFrom(ctx)
if !ok {
    return fmt.Errorf("approval not available")
}
id, err := a.Request(ctx, approval.ApprovalRequest{
    ToolName: "request_human_input",
    Reason:   "请选择数据库",
    Content:  "用 MySQL 还是 PostgreSQL？",
})
// 抛 SuspendError，平台 Resolve 时把 Content 作为工具结果
```

---

## 12. 一句话总结

**`agent/approval` 是 M2.8 异步审批的核心契约层：用 `Approval` 接口抽象"Request 不阻塞 + Resolve one-shot"两条不变量，用 `Immediate` 可选接口兼容旧同步 Confirmer 体验，用 `SuspendError` 把"挂起等审批"表达成 driver 的正常控制流（不是错误而是信号），用 `InMemApproval` 默认实现 + `ConfirmerAdapter` 兼容层覆盖测试 / 本地 / Web / Redis / 数据库所有部署场景。**配合 `RequiredError`（策略侧软标记）和 `WithApproval`/`WithResumeDecisions`（ctx 辅助），把"工具调用前需人工确认"这件事从同步阻塞式升级成了可跨进程、可持久化、可异步接入平台的现代审批流。
