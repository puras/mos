# Checkpoint 挂起点存储（agent/checkpoint）

> 来源：`agent/checkpoint/checkpoint.go`（数据契约 + Store 接口） + `memstore.go`（内存实现） + `filestore.go`（文件实现）。
> 目标读者：理解"异步审批挂起点怎么存、怎么读"的所有人；想实现自定义后端（SQLite/Postgres/Redis）的开发者。

---

## 1. 一句话定位

**`agent/checkpoint` 是 M2.8 异步审批的"挂起点存储层"**：把 driver 在 SuspendError 前写入的"挂起时刻状态快照"抽象成 `Checkpoint` 数据结构 + `Store` 接口，让平台可换后端（SDK 内置内存与文件，数据库由业务侧实现）。

源码顶端的注释（checkpoint.go:1-5）一段话讲清定位：

> "提供断点快照与恢复存储（架构 v2 §3.8 / §10.6 PI SuspendedOperation，参考 LangChain checkpointer）：driver 在任意钩子点挂起（SuspendError）时写入 Checkpoint，**AgentHandle.Resume 加载后从挂起轮续跑**。Store 为可换后端 seam——SDK 内置内存与文件实现，sqlite / postgres 等数据库后端由业务层实现接口并注入（MoS 不操作数据库）"

文件分布：

| 文件 | 职责 |
|---|---|
| `checkpoint.go` | `Checkpoint` 数据结构 + `Store` 接口 + `ErrNotFound` |
| `memstore.go` | `MemStore`：内存实现（单测 / 本地） |
| `filestore.go` | `FileStore`：文件实现（默认后端，原子写） |

---

## 2. 整体协作位置

```
driver.policyGuard
   ↓ 返回 *approval.SuspendError
driver.saveCheckpoint
   ├─ 写 approval/request 事件（durable 审计）
   ├─ Store.Save: Checkpoint{Calls, Request, ApprovalID, SuspendedCallID, Decisions, Snapshot, ...}
   └─ Session.Persistence.SaveState: TaskState 落盘
   ↓
driver 冒泡 SuspendError → Run 返回
   ↓
平台异步：用户决策 → Approval.Resolve
   ↓
AgentHandle.Resume(ctx, decision)
   ├─ Store.Load: 加载 Checkpoint
   ├─ 注入 Decisions[SuspendedCallID] = decision
   ├─ Store.Save: 持久化决策
   ├─ 写 approval/resolve 事件
   └─ Agent.Run → runResumeTurn 直接进入工具批量
   ↓
任务完成 → Store.Delete: 清理 Checkpoint
```

Checkpoint 是这条链路里的"挂起点持久化层"，与 `Session.Persistence`（事件日志 + TaskState）**职责分离**：

| 存储 | 用途 |
|---|---|
| **Checkpoint.Store** | 挂起点（含 Decisions、Snapshot、ScriptState） |
| **Session.Persistence** | 事件日志 + TaskState |

---

## 3. `Checkpoint` 数据结构（checkpoint.go:23-50）

### 3.1 字段分组

```go
type Checkpoint struct {
    // 位置信息
    SessionID string        `json:"session_id"`
    Turn      int           `json:"turn"`
    Step      int           `json:"step"`
    HookKind  hook.HookKind `json:"hook_kind"`

    // 挂起内容
    Calls           []llm.ToolCall                  `json:"calls"`
    Request         approval.ApprovalRequest        `json:"request"`
    ApprovalID      approval.ApprovalID            `json:"approval_id"`
    SuspendedCallID string                          `json:"suspended_call_id"`
    Decisions       map[string]approval.ApprovalDecision `json:"decisions,omitempty"`

    // 状态快照
    Snapshot    session.TaskState      `json:"snapshot"`
    ScriptState *planner.ScriptState   `json:"script_state,omitempty"` // Scripted 专用

    // 时间戳
    CreatedAt  time.Time  `json:"created_at"`
    ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}
```

注释（checkpoint.go:20-22）解释核心三组：

> "Calls：挂起轮的工具调用批（resume 时跳过 LLM 调用，直接重新执行该批）；Request：触发挂起的审批请求（审计 / 平台展示）；SuspendedCallID + Decisions：resume 注入的决策表（approval guard 按 call ID 查表放行）"

### 3.2 字段语义对照

| 字段 | 来源 | Resume 时作用 |
|---|---|---|
| `SessionID` | 挂起时所在会话 | 标识会话 |
| `Turn` / `Step` | driver.runTurn 调用 | 直接跳到该 turn 续跑 |
| `HookKind` | 哪个钩子点挂起 | 审计 |
| `Calls` | 模型的 ToolCalls | **跳过 LLM，直接执行** |
| `Request` | Policy.RequiredError.Request | 平台展示 |
| `ApprovalID` | Approval.Request 返回 | 关联 Resolve 调用 |
| `SuspendedCallID` | 挂起的单条 call.ID | Decisions 键控 |
| `Decisions` | Handle.Resume 注入 | approval guard 查表放行 |
| `Snapshot` | 挂起时 sess.State | 恢复任务进度（含 Facts） |
| `ScriptState` | Scripted 驱动填 | 恢复 outputs/status |
| `CreatedAt` | driver.saveCheckpoint | 审计 |
| `ResolvedAt` | Handle.Resume 注入 | 决策时刻 |

### 3.3 `ScriptState`：Scripted 驱动的专属扩展（checkpoint.go:43-46）

```go
// ScriptState 脚本执行状态快照（Scripted 驱动专用，可选）：
// 脚本 + 已完成步骤输出 + 每步状态。resume 恢复 outputs/status 续跑。
// 老 checkpoint（react/planexecute 等）无此字段，照常可读、恢复语义不变。
ScriptState *planner.ScriptState `json:"script_state,omitempty"`
```

**关键设计点**：

- **可选字段**（`omitempty`）—— 老 checkpoint 没这字段，照常可读
- **驱动专属** —— 其他驱动（react/planexecute）不写此字段，不破坏兼容
- **指针类型** —— 明确区分"有 ScriptState"和"无 ScriptState"

这就是 v2 架构里"驱动扩展点"的设计 —— 在共享 Checkpoint 数据结构里给特定驱动留**可选槽位**，不污染通用契约。

### 3.4 序列化与字段稳定

Checkpoint 的所有字段都带 JSON tag，是**平台持久化契约**。注释（handle.md 也提到）：

> "字段契约变更须同步文档"

加字段 OK（用 `omitempty`），删字段或改 JSON tag 名字就是 break contract。

---

## 4. `Store` 接口（checkpoint.go:54-62）

```go
type Store interface {
    Save(ctx context.Context, ckpt *Checkpoint) error
    Load(ctx context.Context, sessionID string) (*Checkpoint, error)
    Delete(ctx context.Context, sessionID string) error
}
```

注释（checkpoint.go:55-60）逐条解释：

| 方法 | 语义 |
|---|---|
| `Save` | 写入一个 checkpoint（同 sessionID 后写覆盖先写） |
| `Load` | 读取指定会话的**最新** checkpoint；无记录返回 `(nil, ErrNotFound)` |
| `Delete` | 删除指定会话的全部 checkpoint |

### 4.1 「单 checkpoint / 会话」策略

注释（checkpoint.go:56）：

> "同 sessionID 后写覆盖先写"

这是有意简化 —— 一个会话同一时刻只有一个有效挂起点。**新挂起覆盖旧挂起**意味着：

- 二次挂起（如脚本中连续两个审批点）只保留最新
- 旧决策不会污染新挂起
- 实现简单（不需要版本号 / 时间戳排序）

### 4.2 `ErrNotFound`（checkpoint.go:65）

```go
var ErrNotFound = errors.New("checkpoint: not found")
```

Load 时无记录返回此错误。调用方用 `errors.Is(err, checkpoint.ErrNotFound)` 判断"无 checkpoint = 普通续跑"。

---

## 5. `MemStore`：内存实现（memstore.go）

### 5.1 数据结构

```go
type MemStore struct {
    mu      sync.Mutex
    entries map[string]*Checkpoint
}
```

**简单 map**：sessionID → 最新 checkpoint，加锁保护并发。

### 5.2 关键方法

#### `Save`（memstore.go:22-31）

```go
func (m *MemStore) Save(ctx context.Context, ckpt *Checkpoint) error {
    if m == nil || ckpt == nil {
        return errors.New("checkpoint: nil store or checkpoint")
    }
    m.mu.Lock()
    defer m.mu.Unlock()
    cp := *ckpt                     // 复制一份
    m.entries[ckpt.SessionID] = &cp
    return nil
}
```

**注意 `cp := *ckpt`** —— 拷贝一份再存。**避免外部修改** ckpt 后影响 store 里的快照（driver 后续可能修改 ckpt 比如填 ResolvedAt）。

#### `Load`（memstore.go:34-46）

```go
func (m *MemStore) Load(ctx context.Context, sessionID string) (*Checkpoint, error) {
    if m == nil { return nil, ErrNotFound }
    m.mu.Lock()
    defer m.mu.Unlock()
    ckpt, ok := m.entries[sessionID]
    if !ok { return nil, ErrNotFound }
    cp := *ckpt                     // 拷贝返回
    return &cp, nil
}
```

同样 `cp := *ckpt` 复制返回，**避免外部修改 store 内部数据**。

#### `Delete`（memstore.go:49-57）

`m == nil` 返回 nil（不报错），方便调用方无脑调。

### 5.3 nil 安全

整个 MemStore 对 nil receiver 安全（Load 返回 ErrNotFound、Save 返回错误、Delete 返回 nil），让单测和无 store 配置场景下不 panic。

---

## 6. `FileStore`：文件实现（filestore.go）

### 6.1 文件布局

```
<dir>/
  <sessionID>.checkpoint.json   # 单文件，原子写
```

注释（filestore.go:12-14）：

> "每会话单文件 `<dir>/<sessionID>.checkpoint.json`。写入走 tmp + Rename 原子化（**并发/崩溃下不产生半写文件**）。数据库后端由业务层实现 Store 注入"

### 6.2 `Save` 原子写（filestore.go:33-53）

```go
func (f *FileStore) Save(ctx context.Context, ckpt *Checkpoint) error {
    if ckpt == nil { return errors.New("checkpoint: nil checkpoint") }
    if err := os.MkdirAll(f.dir, 0o755); err != nil { return ... }
    
    b, err := json.MarshalIndent(ckpt, "", "  ")  // 缩进，便于人工查
    if err != nil { return ... }
    
    path := f.path(ckpt.SessionID)
    tmp := path + ".tmp"
    if err := os.WriteFile(tmp, b, 0o644); err != nil { return ... }
    if err := os.Rename(tmp, path); err != nil { return ... }
    return nil
}
```

**三步走**：

1. **懒创建目录** —— `os.MkdirAll` 在 Save 时才创建目录（NewFileStore 不创建）
2. **写 tmp 文件** —— 写到 `path + ".tmp"`，不直接写目标文件
3. **Rename 替换** —— `os.Rename` 是原子操作，崩溃/并发不会产生半写文件

**为什么用 `json.MarshalIndent`** —— 注释没写但从用法可看出：便于**人工排查** checkpoint 内容。生产环境的 checkpoint 是 JSON，可读性 > 文件大小。

### 6.3 `Load`（filestore.go:56-69）

```go
func (f *FileStore) Load(ctx context.Context, sessionID string) (*Checkpoint, error) {
    b, err := os.ReadFile(f.path(sessionID))
    if err != nil {
        if os.IsNotExist(err) { return nil, ErrNotFound }
        return nil, fmt.Errorf("checkpoint: read: %w", err)
    }
    var ckpt Checkpoint
    if err := json.Unmarshal(b, &ckpt); err != nil { return ... }
    return &ckpt, nil
}
```

**`os.IsNotExist` 转 `ErrNotFound`** —— 让调用方能统一用 `errors.Is(err, ErrNotFound)` 判别，**无关心后端**。

### 6.4 `Delete` 幂等（filestore.go:72-78）

```go
func (f *FileStore) Delete(ctx context.Context, sessionID string) error {
    err := os.Remove(f.path(sessionID))
    if err != nil && !os.IsNotExist(err) { return ... }
    return nil
}
```

**不存在不算错** —— 注释：`"os.Remove error + !os.IsNotExist"` 才报错，让 Delete 可重入。

---

## 7. MemStore vs FileStore 对照

| 维度 | MemStore | FileStore |
|---|---|---|
| 持久化 | 否（进程重启数据丢失） | 是（落盘） |
| 跨进程共享 | 否 | 是（同文件系统） |
| 并发安全 | ✅ 互斥锁 | ✅ 文件系统原子操作 |
| 崩溃安全 | 不适用 | ✅ tmp + Rename 原子写 |
| 实现复杂度 | 极简（map + 锁） | 中等（文件 IO + 原子写） |
| 适用场景 | 单测 / 本地调试 | 默认后端 / 简单部署 |
| 跨机器 | 不支持 | 同文件系统才支持 |

---

## 8. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **后写覆盖先写** | Store 接口语义 | 单 checkpoint / 会话 |
| **`ErrNotFound` 统一判别** | MemStore / FileStore 都返回 | 调用方用 errors.Is 判别 |
| **Save 拷贝入存** | MemStore `cp := *ckpt` | 防外部修改 |
| **Load 拷贝返回** | MemStore `cp := *ckpt` | 防外部修改 |
| **Delete 不存在不算错** | FileStore `!os.IsNotExist` | 可重入 |
| **Save 原子写（不产生半写）** | FileStore tmp + Rename | 崩溃/并发安全 |
| **ScriptState 可选字段** | `omitempty` + 指针 | 兼容老 checkpoint |
| **JSON tag 是持久化契约** | 所有字段带 tag | 改字段须同步文档 |
| **不实现 Immediate 默认** | 同 approval 包 | 设计原则一致 |
| **nil receiver 安全** | MemStore 各方法 | 装配友好 |

---

## 9. 与 Session.Persistence 的职责分离

这两个都是持久化层，**但负责完全不同的东西**：

```
┌──────────────────────────────────────────────────────────────┐
│              持久化层（两个独立设施）                           │
├──────────────────────────────┬───────────────────────────────┤
│   session.Persistence        │     checkpoint.Store          │
├──────────────────────────────┼───────────────────────────────┤
│ • 事件日志（append-only）      │ • 挂起点快照（覆盖写）         │
│ • TaskState（Facts/Status）   │ • Decisions 决策表            │
│ • JSONL / DB / 自定义         │ • Snapshot / ScriptState     │
│ 写：sess.Append 同步落盘       │ 写：driver.saveCheckpoint     │
│ 读：Restore 重放              │ 读：Handle.Resume 加载        │
│                              │                               │
│ 用途：恢复会话本身             │ 用途：恢复挂起时刻             │
└──────────────────────────────┴───────────────────────────────┘
```

**为什么不合并到一个 Store？**

| 维度 | Session.Persistence | Checkpoint.Store |
|---|---|---|
| 写入时机 | 每次事件 Append（高频） | 每次挂起（低频） |
| 写入模式 | 追加写 | 覆盖写 |
| 数据特征 | append-only 日志 | 单点快照 |
| 生命周期 | 整个会话 | 仅挂起时刻存在 |

**两个独立的接口让实现可以选择不同后端**：事件日志用 JSONL/SQLite，checkpoint 用 Redis/内存，按场景优化。

---

## 10. 自定义扩展姿势

### 10.1 SQLite 实现（推荐生产）

```go
type SQLiteStore struct {
    db *sql.DB
}

func (s *SQLiteStore) Save(ctx context.Context, ckpt *Checkpoint) error {
    data, _ := json.Marshal(ckpt)
    _, err := s.db.ExecContext(ctx,
        `INSERT INTO checkpoints (session_id, data, created_at, resolved_at)
         VALUES (?, ?, ?, ?)
         ON CONFLICT(session_id) DO UPDATE SET data=excluded.data`,
        ckpt.SessionID, data, ckpt.CreatedAt, ckpt.ResolvedAt)
    return err
}

func (s *SQLiteStore) Load(ctx context.Context, sessionID string) (*Checkpoint, error) {
    var data []byte
    err := s.db.QueryRowContext(ctx,
        `SELECT data FROM checkpoints WHERE session_id = ?`, sessionID).Scan(&data)
    if err == sql.ErrNoRows { return nil, ErrNotFound }
    if err != nil { return nil, err }
    
    var ckpt Checkpoint
    if err := json.Unmarshal(data, &ckpt); err != nil { return nil, err }
    return &ckpt, nil
}

func (s *SQLiteStore) Delete(ctx context.Context, sessionID string) error {
    _, err := s.db.ExecContext(ctx,
        `DELETE FROM checkpoints WHERE session_id = ?`, sessionID)
    return err
}
```

### 10.2 Redis 实现（分布式平台）

```go
type RedisStore struct {
    rdb *redis.Client
}

func (r *RedisStore) Save(ctx context.Context, ckpt *Checkpoint) error {
    data, _ := json.Marshal(ckpt)
    return r.rdb.Set(ctx, "ckpt:"+ckpt.SessionID, data, 7*24*time.Hour).Err()
}

func (r *RedisStore) Load(ctx context.Context, sessionID string) (*Checkpoint, error) {
    data, err := r.rdb.Get(ctx, "ckpt:"+sessionID).Bytes()
    if err == redis.Nil { return nil, ErrNotFound }
    if err != nil { return nil, err }
    
    var ckpt Checkpoint
    if err := json.Unmarshal(data, &ckpt); err != nil { return nil, err }
    return &ckpt, nil
}

func (r *RedisStore) Delete(ctx context.Context, sessionID string) error {
    return r.rdb.Del(ctx, "ckpt:"+sessionID).Err()
}
```

### 10.3 加 ScriptState 字段（驱动扩展）

```go
// 在 driver/scripted/scripted.go 的 saveCheckpoint 里：
ckpt := &checkpoint.Checkpoint{
    SessionID: sess.ID,
    Turn: turn, Step: step,
    HookKind: hook.HookToolPreExecute,
    Calls: calls, Request: req, ApprovalID: id, SuspendedCallID: call.ID,
    Decisions: map[string]approval.ApprovalDecision{},
    Snapshot: sess.State,
    ScriptState: &planner.ScriptState{        // ← Scripted 专属
        Script: currentScript,
        StepOutputs: outputs,
        StepStatuses: statuses,
    },
    CreatedAt: time.Now(),
}
rt.Checkpoints.Save(ctx, ckpt)
```

`ScriptState` 是 `omitempty` 指针，老 Checkpoint（react/planexecute）没这字段，照常可读。

### 10.4 用 Load 时检查 ResolvedAt（决定续跑策略）

```go
ckpt, err := store.Load(ctx, sessionID)
if errors.Is(err, checkpoint.ErrNotFound) {
    // 无 checkpoint = 普通续跑
} else if err != nil {
    return err
}
if ckpt.ResolvedAt != nil {
    // 已决策 → 普通续跑
} else {
    // 未决策 → 等待决策
}
```

### 10.5 在 Handle.Resume 中清理 checkpoint

```go
// 任务完成后
if h.Store != nil {
    _ = h.Store.Delete(ctx, h.Session.ID)
}
```

注意：`Store.Delete` 要幂等（FileStore 已实现），即使之前已 Delete 过也无害。

---

## 11. 一句话总结

**`agent/checkpoint` 是 M2.8 异步审批的"挂起点存储层"：用 `Checkpoint` 数据结构把"位置信息 + 挂起内容 + 状态快照 + 时间戳"打包成单一不可变快照，用 `Store` 接口（Save/Load/Delete）抽象可换后端（MemStore/FileStore/业务自定义），用「单 checkpoint / 会话」策略 + 原子写 + Decisions 键控放行让"挂起 → 决策 → 续跑"完整闭环，用 `ScriptState` 可选字段给 Scripted 驱动留专属扩展槽位而不污染通用契约。** 它与 `Session.Persistence` 职责分离（事件日志 vs 挂起快照），让两个层可以独立选择最合适的后端，是 MoS "持久化可换 seam" 设计哲学的具体落地。