# 会话子系统（agent/session）

> 来源：`agent/session/session.go` + `message.go` + `state.go` + `persistence.go` + `jsonl.go`（架构 v2 §3.2 / §10.3）。
> 目标读者：理解"会话日志是唯一事实来源"的工程师；做持久化/恢复/压缩/审计的开发者。

---

## 1. 一句话定位

**`agent/session` 是 agent 的"记忆 + 投影 + 持久化"三层一体的子系统**：事件日志（append-only JSONL）是唯一事实来源，模型消息历史是日志的投影，外部任务状态独立于消息存在。三者各司其职，互不污染。

文件分布：

| 文件 | 职责 |
|---|---|
| `session.go` | `Session` 容器、事件 Append、消息投影（`deriveMessages`）、快照 Save/Restore |
| `message.go` | 两层消息结构（`InternalMessage` vs `llm.Message`）、翻译边界 |
| `state.go` | 外部可信任务状态（`TaskState`、Fact、Status 状态机） |
| `persistence.go` | 持久化接口 + 崩溃恢复（`RepairInterrupted`） |
| `jsonl.go` | `Persistence` 的默认 JSONL 实现 |

---

## 2. 核心设计哲学：「日志即事实」

源码注释多次强调（session.go:72、persistence.go:16）：

> "事件日志是唯一事实来源：任何进入模型请求的消息必须能从 Events 重建（不变式）"
> "恢复 = 日志重放"

这句话有四层含义：

1. **追加写**：事件只能 `Append`，不能原地改写
2. **投影派生**：模型消息历史 = `deriveMessages(Events)`，无独立存储
3. **重放恢复**：跨进程恢复 = 重放事件日志 → 重新投影
4. **不可丢**：持久化失败时事件**不进内存**（fail-fast），不破坏"日志即事实"

这套哲学贯穿整个 agent 层 —— driver/system/event/auditor/checkpoint 都依赖这条不变量。

---

## 3. `Session` 数据结构

### 3.1 字段总览

```go
type Session struct {
    ID     string         `json:"id"`
    Events []SessionEvent `json:"events"`         // append-only 事件日志
    State  TaskState      `json:"state"`          // 外部可信任务状态
    Header SessionHeader  `json:"header,omitempty"` // cwd / lineage / seed 等元数据

    Persistence Persistence `json:"-"`   // 可选持久化后端（nil = 纯内存）
    persisted   bool        `json:"-"`   // 内部状态：header 是否已落盘

    TurnSkills     []ActivatedSkill `json:"-"`  // 本次 Run 激活的 Skill
    TurnToolSubset []string         `json:"-"`  // 本次 Run 的工具白名单并集
}
```

**关键设计**：

- `Persistence` 不序列化（`json:"-"`）—— 后端实例不可跨进程传递
- `TurnSkills` / `TurnToolSubset` 也不序列化 —— **运行时态**，每次 Run 重置
- `Events` 是唯一持久的"叙事"，其它字段都是它的衍生或伴生

### 3.2 `ActivatedSkill`：跨包依赖规避

注释里写得很直白（session.go:49-51）：

> "Skill 完整定义在 agent 包（不可在此直接引用，避免 agent↔session 循环依赖）；这里存的是激活 Skill 的最小切片（Name + AllowedTools）"

这是典型的**包依赖解耦技巧**：session 包只持有"激活 Skill 的最小必要信息"，具体内容由 `agent.AddTurnSkill` 投影写入。下游需要完整 Skill 定义时，回到 agent 包查。

---

## 4. 事件日志：`SessionEvent` 与 Append

### 4.1 事件结构

```go
type SessionEvent struct {
    Seq  int64           `json:"seq"`   // 1 起连续编号
    Type string          `json:"type"`  // 事件类型（event.SessionEvent*）
    Data json.RawMessage `json:"data"`  // 类型相关负载（json 编码）
}
```

**Seq 是单调连续的**，由 `Append` 自动分配（`int64(len(s.Events)) + 1`，session.go:77）。

### 4.2 `Append` 的双重写入

```go
func (s *Session) Append(typ string, data any) error {
    ev := SessionEvent{Seq: ..., Type: typ, Data: event.MarshalData(data)}
    if s.Persistence != nil {
        if !s.persisted {
            if err := s.Persistence.SaveHeader(s.ID, s.Header); err != nil { return ... }
            s.persisted = true
        }
        if err := s.Persistence.Append(s.ID, ev); err != nil { return ... }
    }
    s.Events = append(s.Events, ev)
    return nil
}
```

写入顺序：**先落盘，再入内存**。如果落盘失败，事件**不会进内存**，保证两个视图一致。

Header 在首次 Append 时按需写入并标记 `persisted = true`，后续不再写。

### 4.3 类型化 helper

为避免驱动到处写 `event.SessionEventUserMessage` / `event.UserMessageData{...}`，session 包提供：

```go
func (s *Session) AppendUserMessage(message, source string) error
func (s *Session) AppendAssistantMessage(turn int, content, reasoning string, calls []llm.ToolCall, usage llm.Usage) error
func (s *Session) AppendToolCall(turn int, call *llm.ToolCall) error
func (s *Session) AppendToolResult(turn int, call *llm.ToolCall, result any, text string, err error) error
```

### 4.4 `NextTurn` 全局唯一

```go
func (s *Session) NextTurn() int
```

注释（session.go:280-282）解释：

> "已有 turn/start 事件时取最大 Turn + 1（续跑会话 / 多步骤执行时全局递增），否则从 1 开始"

关键点：**Turn 是全局递增的，不是按 Run 重置**。这保证了：
- 同一会话多次 Run 的 Turn 唯一（不会重复计数）
- 跨 Run 的工具/审计事件能按 Turn 串成完整叙事
- Resume 时 Turn 不冲突

### 4.5 `DropInterruptedForTurn`：M2.8 刻意挂起恢复

```go
func DropInterruptedForTurn(s *Session, turn int)
```

这是一个**反操作 helper**：移除 RepairInterrupted 合成的 `turn/end{reason: interrupted}`（session.go:316-319）。

注释强调了一个微妙语义：

> "挂起轮的 turn/end 应在续跑完成时才写入，不应由崩溃恢复逻辑提前合成。调用方应仅在确认该 turn 确有 checkpoint（刻意挂起）时使用；崩溃恢复场景不调用。"

也就是说：
- **崩溃恢复**：让 `turn/end{interrupted}` 保留，标记该轮未完成
- **刻意挂起 + 续跑成功**：用 `DropInterruptedForTurn` 抹掉合成的 interrupted，再补写真正的 end

---

## 5. 消息投影：`deriveMessages`

### 5.1 投影规则（session.go:101-201）

| 事件类型 | 投影为 |
|---|---|
| `user/message` | `UserMsg`（保留 Source 标注） |
| `assistant/message` | `AssistantMsg` 或 `AssistantToolMsg`（按 ToolCalls 区分） |
| `assistant/chunk` | 流式增量累积；同轮存在 `assistant/message` 时被消费；异常中断时聚合为消息 |
| `tool/result` | `ToolMsg`（优先 Text，否则 `FormatToolResult(Result)`） |
| `compaction/summary` | `SummaryMsg` 替换阴影范围（详见 §7） |

### 5.2 「同轮 chunk 与 message 共存」语义

注释（session.go:106-107）写得很细：

> "流式增量累积；同轮存在 assistant/message 时被其消费，仅当一轮以 chunk 结尾（异常中断）时聚合为消息"

实现：维护 `pendingChunk` 累加 `assistant/chunk` 的 Delta，遇到 `assistant/message` 时**清空**（"完整消息消费掉前置 chunk"）；只有异常中断（轮结束还没见到 `assistant/message`）时才 `flushChunk()` 把累积内容聚合为消息。

### 5.3 阴影替换：compaction/summary

压缩摘要事件携带 `ShadowedFrom`/`ShadowedTo`：

```go
for seq := d.ShadowedFrom; seq <= d.ShadowedTo; seq++ {
    shadowed[seq] = true
}
summaries = append(summaries, messageAt{pos: d.ShadowedFrom, msg: SummaryMsg(d.Summary)})
```

投影时跳过阴影内的事件，把摘要以 `pos = ShadowedFrom` 插入，**按位置稳定排序**（`stableSortByPos`，插入排序实现 —— 注释解释"规模小"）。

这就是**"摘要替换旧消息，可回放"**的精妙之处：摘要事件本身就是事件日志的一部分，回放时自然还原"哪些消息被压了、摘要插哪"。

### 5.4 `MessageEventSeqs` 给压缩器用

```go
func MessageEventSeqs(events []SessionEvent) []int64
```

返回产生模型面消息的事件序号（`user/message`、`assistant/message`、`tool/result`、`compaction/summary`），用于压缩器计算阴影范围。**`assistant/chunk` 不算**（已经被 `assistant/message` 消费）。

---

## 6. 两层消息结构与翻译边界

### 6.1 为什么需要两层？（M2.4 翻译边界）

源码注释（message.go:5-15）解释：

> "InternalMessage 是 Agent 内部的两层消息结构中的第一层（M2.4 翻译边界）。相比 llm.Message（LLM 标准消息，第二层），附加运行期元数据"

| 字段 | 类型 | 作用 |
|---|---|---|
| `llm.Message` | 嵌入 | LLM 标准消息（role/content/tool_calls 等） |
| `ExcludeFromContext` | bool | 不进模型上下文，仅留会话历史（审计/落盘） |
| `Summary` | bool | 历史摘要消息，统一转 user 角色注入 |
| `Source` | string | 消息来源标注（观测/调试） |

### 6.2 `ConvertToLlm` 翻译边界

```go
func ConvertToLlm(msgs []InternalMessage) []llm.Message
```

规则（message.go:60-67）：
1. **过滤** `ExcludeFromContext` 的消息（不进模型）
2. **原样转换** 标准角色（system/user/assistant/tool）
3. **统一转 user** 摘要与自定义消息（按来源标注前缀）

### 6.3 翻译规则的妙处

- **审计消息不进模型**：可以往日志里写"工具被拒绝"等内部事件，但不让模型看到（避免它被自己的审计吓到）
- **摘要统一转 user**：避免厂商对 summary 角色支持不一致，统一前缀 `[历史摘要]\n` 让模型理解
- **自定义消息也转 user**：应用层注入的额外上下文（Agent 间通信等）也按 source 前缀标注

---

## 7. 外部任务状态：`TaskState`（架构 v2 §3.2）

### 7.1 设计动机：审计式状态迁移

注释（state.go:50-52）：

> "TaskState 是外部可信任务状态（single source of truth），独立于消息历史存在。对应长程 Agent 的'审计式状态迁移'：只有经 Auditor 独立验证的事实才能写入 Facts，模型自述不算数"

也就是说：

| 存储 | 信任级别 | 写入者 |
|---|---|---|
| 消息历史（Events） | 低（模型自述也在内） | 模型 + 工具 |
| **TaskState.Facts** | **高（环境独立验证）** | **Auditor only** |

这是**纵深防御的另一层**：就算模型幻觉污染了消息，TaskState 仍然是真相。

### 7.2 数据结构

```go
type TaskState struct {
    Goal    string     `json:"goal"`              // 任务目标（首个输入）
    Facts   []Fact     `json:"facts,omitempty"`   // 已验证事实（审计报告）
    Pending []string   `json:"pending,omitempty"` // 待办（Manager 角色维护）
    Blocked []string   `json:"blocked,omitempty"` // 阻塞项
    Status  TaskStatus `json:"status"`            // 状态机
}

type Fact struct {
    Summary  string  // 简短摘要（工具名 + 关键结果）
    Evidence string  // 环境证据（工具原始输出）
    Tool     string  // 产生事实的工具名
}
```

### 7.3 状态机

```go
const (
    StatusPending    // 已创建未开始
    StatusRunning    // 执行中
    StatusBlocked    // 阻塞（MaxTurns 保险丝 / Doom Loop 致命错误）
    StatusCompleted  // 正常完成
)
```

**JSON 序列化用字符串**（pending/running/blocked/completed），不是数字 —— 这是平台持久化契约（sdk-contracts.md §3）的一部分。`UnmarshalJSON` 同时兼容字符串和数字，便于老数据迁移（state.go:73-96）。

### 7.4 `AddFact` 去重

```go
func (s *TaskState) AddFact(f Fact) {
    for _, exist := range s.Facts {
        if exist.Tool == f.Tool && exist.Evidence == f.Evidence {
            return
        }
    }
    s.Facts = append(s.Facts, f)
}
```

**幂等判据：Tool + Evidence 相同即重复**。Summary 故意不参与（同一工具同证据可以有多条不同摘要的解读）。配合 `RunContext.AddFact` 的互斥锁（详见 driver.md §5），并发安全。

### 7.5 `Snapshot` 文本摘要

```go
func (s *TaskState) Snapshot() string
```

返回多行文本：`任务目标 / 任务状态 / 待办 / 阻塞 / 已验证事实`。用途：
- **fresh-context 压缩**（system.md §3 提到的 SlotState 槽位）
- **审计报告**（人工查阅时只看 Facts）

---

## 8. 持久化抽象：`Persistence`

### 8.1 接口契约（persistence.go:19-36）

```go
type Persistence interface {
    SaveHeader(sessionID string, h SessionHeader) error
    LoadHeader(sessionID string) (SessionHeader, error)
    Append(sessionID string, ev SessionEvent) error
    Load(sessionID string) ([]SessionEvent, error)
    SaveState(sessionID string, st TaskState) error
    LoadState(sessionID string) (TaskState, error)
    Close() error
}
```

注释（persistence.go:14-18）：

> "backend 可换——jsonl 由 SDK 内置（纯文件）；sqlite / postgres 等数据库后端由业务层实现本接口并注入（MoS 不操作数据库）"

**设计原则**：MoS 不绑死数据库。JSONL 是默认实现够用，业务想要 sqlite/postgres 时自己实现接口注入。

### 8.2 Header 与事件日志分离

- **Header**（version/cwd/lineage/seed）：不进事件流，独立存储
- **事件流**：append-only，按 Seq 顺序

注释明确指出："格式版本 / cwd / lineage / seed 不进事件流" —— 元数据变化不影响事件流，可以原地更新 header 而不污染日志。

### 8.3 `ErrNotFound` 语义

`ErrNotFound` 表示**会话不存在（无持久化记录）**；`Restore` 按空会话处理，不报错。

这意味着"第一次跑的新会话"与"恢复不存在的会话"行为一致 —— 调用方不需要区分。

### 8.4 `Restore` 完整流程

```
LoadHeader（不存在 → 空 Header）
   ↓
Load（事件日志）
   ↓
RepairInterrupted（孤儿 turn/start 补 turn/end{interrupted}）
   ↓
LoadState（不存在 → 空 TaskState）
   ↓
构造 Session { ID, Events, Header, State }
```

### 8.5 `RepairInterrupted`：崩溃恢复（v2 §10.3）

注释（persistence.go:75-78）写得很讲究：

> "按 turn 号集合求差判定孤儿（而非栈配对），可正确处理嵌套/乱序的 turn 号：例：start(1) → start(2) → end(1) 时，孤儿是 turn 2 而非 turn 1"

实现：扫描所有 `turn/start` 收集 `started` 集合，扫描所有 `turn/end` 收集 `ended` 集合，差集 = 孤儿，逐个补 `turn/end{interrupted}`。

**故意保留 `interrupted` 而不截断**，让上游能识别"哪些轮次是崩溃导致中断的，哪些是刻意挂起的"。

---

## 9. JSONL 后端实现

### 9.1 文件布局

```
<dir>/
  <sessionID>.jsonl       # 事件流 + 首行 Header
  <sessionID>.state.json  # 任务状态（独立文件，原子写）
```

每行 `jsonlLine` 二选一：`{header: ...}` 或 `{event: ...}`。注释（jsonl.go:23）：

> "事件顺序即磁盘顺序（Seq 连续），崩溃时尾部半行会被 Load 跳过（容忍截断写）"

### 9.2 容错读：`read` 函数（jsonl.go:145-182）

- 文件不存在 → 返回 `nil, nil, nil`（视为空会话）
- 空行 → 跳过
- JSON 解析失败 → `continue`（容忍损坏半行，注释："崩溃截断写"）
- 缓冲区：初始 64KB，最大 4MB（适配长事件如工具输出）

### 9.3 `SaveState` 原子写

```go
tmp := path + ".tmp"
os.WriteFile(tmp, b, 0o644)
os.Rename(tmp, path)  // 原子
```

**崩溃不产生半写文件**。注释（jsonl.go:97-98）：

> "原子写：tmp + Rename，崩溃不产生半写文件"

而事件流不是原子的（`OpenFile + Write`）—— 但崩溃只会丢尾部事件，**不会污染已落盘的事件**，因为是追加写。

### 9.4 `Close` 是 no-op

JSONL 没有长连接/文件句柄缓存，`Close()` 直接返回 nil。注释："JSONL 无长期资源，no-op"。

---

## 10. `Save` / `RestoreSession`：跨进程序列化

### 10.1 稳定序列化契约

```go
type sessionSnapshot struct {
    ID     string         `json:"id"`
    Events []SessionEvent `json:"events"`
    State  TaskState      `json:"state"`
    Header SessionHeader  `json:"header,omitempty"`
}
```

注释（session.go:233-234）：

> "Session 的稳定序列化结构（平台持久化契约，见 docs/sdk-contracts.md §3）。字段契约变更须同步文档"

**这是给平台用的契约**，不是 SDK 内部的备忘录。改字段就是 break contract。

### 10.2 不序列化什么

- `Persistence`（后端实例不可跨进程）
- `TurnSkills` / `TurnToolSubset`（运行时态）
- `persisted`（内部标志）

### 10.3 恢复后的状态

注释（session.go:253-254）：

> "恢复后的会话可直接传给 Driver.Run 续跑。**只序列化数据；Agent 配置（模型对象 / 工具集 / 策略）由平台自行持有**，恢复会话后需重建 Agent 再 Run 续跑"

这是个关键约束：**会话数据 ≠ Agent 配置**。平台持久化只存 Session，业务侧负责保存"用哪个模型、哪些工具、什么策略"，Resume 时重建 Agent。

### 10.4 `Header.Version` 自动补齐

```go
if sess.Header.Version == 0 {
    sess.Header.Version = sessionFormatVersion
}
```

老版本会话（没有 Version 字段）恢复时自动填当前版本，向前兼容。

---

## 11. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **日志即事实** | `Append` 先落盘再入内存 | 任何恢复路径都不会丢事件 |
| **消息可重放** | `deriveMessages(Events)` 纯函数 | 无独立消息存储 |
| **Seq 单调连续** | `int64(len(s.Events))+1` | 持久化层可重放 |
| **Turn 全局唯一** | `NextTurn` 扫最大 Turn + 1 | 跨 Run 不冲突 |
| **失败 fail-fast** | `Append` 落盘失败返回错误 | 不破坏日志一致性 |
| **审计事实独立** | `TaskState.Facts` 仅 Auditor 写入 | 模型幻觉不污染事实 |
| **Facts 幂等去重** | `Tool + Evidence` 相同即跳 | 并发安全配合互斥锁 |
| **状态机可序列化** | `MarshalJSON` 用字符串 | 平台契约稳定 |
| **JSONL 截断容忍** | `read` 函数跳过损坏行 | 崩溃不污染已落盘数据 |
| **状态原子写** | tmp + Rename | 崩溃不产生半写文件 |
| **崩溃/刻意挂起可区分** | `interrupted` 保留 + `DropInterruptedForTurn` | Resume 语义正确 |

---

## 12. 自定义扩展姿势

### 12.1 实现自定义 `Persistence`（接数据库）

```go
type MyDB struct{ db *sql.DB }

func (m *MyDB) SaveHeader(id string, h session.SessionHeader) error { ... }
func (m *MyDB) LoadHeader(id string) (session.SessionHeader, error) { ... }
func (m *MyDB) Append(id string, ev session.SessionEvent) error { ... }
func (m *MyDB) Load(id string) ([]session.SessionEvent, error) { ... }
func (m *MyDB) SaveState(id string, st session.TaskState) error { ... }
func (m *MyDB) LoadState(id string) (session.TaskState, error) { ... }
func (m *MyDB) Close() error { ... }

// 注入
sess := session.NewSession("xxx")
sess.Persistence = &MyDB{db: db}
```

要求：
- `Append` 必须按 Seq 顺序落盘
- `Load` 按 Seq 升序返回
- 失败时返回错误，`Append` 会拒绝入内存

### 12.2 自定义事件类型

```go
const event.MyCustomType = "my/custom"

sess.Append(event.MyCustomType, event.MarshalData(myPayload))
```

投影时如果想让它出现在 `deriveMessages` 输出里，需要扩 `session.go` 的 switch（默认不会被识别，会被当作未知事件忽略 —— 这是 feature，避免误把审计事件投影给模型）。

### 12.3 自定义消息角色

通过 `CustomMsg(source, content)` 走翻译边界，统一转 user 角色（message.go:50-53）。不要直接构造 `llm.Message{Role: "..."}` —— 会绕过内部 Source 标注。

---

## 13. 一句话总结

**`agent/session` 是 agent 的"长期记忆 + 投影层 + 持久化适配器"三位一体：用 append-only JSONL 事件日志作为唯一事实来源，把模型消息历史作为日志的投影函数（可重放），把可信任务状态作为独立审计轴（抗幻觉），把持久化做成可换后端接口（JSONL/SQLite/Postgres 任选），用「日志即事实 / 失败 fail-fast / 崩溃可恢复 / 摘要可回放」四条不变量串起整个子系统。**