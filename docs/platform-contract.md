# MoS 平台契约（事件 JSON / 状态序列化 / 并发）

> 目的：把 MoS 作为 SDK 暴露给**产品层**（Web 控制台、CLI、SaaS 平台）的稳定契约写死。
> 本文的消费方是 `mos-web`（本地 Web 控制台，独立仓库，通过 `go.mod` 依赖本 SDK）。
>
> 与旧版 `docs/sdk-contracts.md`（`main_bak1015` 分支，已随重建删除）的关系：
> 旧文描述的是 v2 之前的 `{"seq","type","data"}` 嵌套事件与 `Observer` 回调；
> 重建后的事件格式已改为**扁平** `{"seq","kind",...}`，本文是它的替代者。
> 旧文里仍然有效的那条原则被提升为本文 §4 的硬约束。

---

## 1. 职责边界（重申）

| 层 | 归属 | 职责 |
| --- | --- | --- |
| MoS | 本仓库（SDK） | 事件日志、消息投影、循环驱动、工具执行、守卫/钩子、审批编排、文件 jail、进程沙箱 |
| 产品层 | 独立仓库（如 `mos-web`） | HTTP/SSE 服务端点、鉴权、会话索引、前端渲染、人工确认通道的具体形态 |

**MoS 明确不做**：HTTP 服务、SSE 封装、Web UI、鉴权、多租户、计费、数据库。
产品层通过 SDK 注入的能力（`approval.Answerer`、`session.Persistence`、`llm.ChatModel`）
与事件流承担这些职责。本文档只定义**产品层可以依赖的契约**，不规定产品层怎么实现。

---

## 2. 事件 JSON 契约

### 2.1 事件对象

会话日志的每条记录是一个扁平 JSON 对象（`session.Event`）：

```jsonc
{
  "seq": 12,                       // 从 1 起连续递增，append-only，不可改写
  "kind": "assistant",             // 事件类型（见 §2.2 目录）
  "turn": 3,                       // 所属轮次（turn/start 起的轮次号）
  "time": "2026-10-26T08:00:00Z",  // 落盘时刻（UTC，RFC3339）
  "text": "...",                   // user / assistant 的正文
  "reasoning": "...",              // assistant 的思维链（thinking 模式）
  "toolCalls": [                   // assistant 发起的工具调用
    {"id": "call_1", "name": "read_file", "args": "{\"path\":\"a.txt\"}"}
  ],
  "toolCall": {                    // tool/call 的载荷
    "id": "call_1", "name": "read_file", "args": "{\"path\":\"a.txt\"}"
  },
  "toolResult": {                  // tool/result 的载荷
    "callId": "call_1",
    "content": "{\"lines\": [...]}",
    "isError": false               // 显式失败：守卫拦截 / 工具报错 / 参数非法
  },
  "usage": {                       // assistant 的模型调用用量
    "prompt_tokens": 1200,
    "fresh_input_tokens": 300,     // 去缓存口径（跨协议可比，见 §2.4）
    "completion_tokens": 400,
    "total_tokens": 1600,
    "details": {"input": {"cache_read": 900, "cache_write": 0}, "output": {"reasoning": 0}}
  },
  "reason": "completed",           // turn/end 的分类：completed|terminated|error|interrupted
  "error": "...",                  // turn/end 的失败详情（分类之外的诊断信息）
  "approval": {                    // approval/* 的载荷
    "id": "ap-xxx",                // 审批请求 id（asked 与 decided 配对）
    "toolName": "write_file",
    "callId": "call_1",            // 对应 tool/call 事件的 CallID
    "reason": "...",               // asked 才有：为什么要人工确认
    "outcome": "approved"          // decided 才有：approved|rejected|cancelled|unavailable
  }
}
```

**字段规则**（产品层依赖的不变量）：

- `seq` **从 1 起连续递增**，由 `Session.Append` 统一分配；产品层可直接拿它当
  SSE 的 `id:` 与断点续传的游标。
- `kind` 是事件类型；其余字段按 `kind` 取用，**不适用的字段一律省略**（`omitempty`）。
  产品层解析时**不得假设某字段一定存在**。
- `time` 是 RFC3339（UTC）。
- `toolCall.args` 与 `toolResult.content` 是**字符串**：前者是模型给的参数 JSON，
  后者是回填给模型的文本（成功时是工具返回值的 JSON 编码，失败时是失败原因）。
- 事件对象**只增不改**：崩溃恢复只补事件，不回写已有事件（§3.2）。

### 2.2 事件目录（预算制）

| kind | 载荷 | 投影为模型消息 | 说明 |
| --- | --- | --- | --- |
| `turn/start` | `turn` | ✗ | 一轮开始 |
| `turn/end` | `turn` `reason` `error` | ✗ | 一轮结束；`reason` 是分类，`error` 是详情（只进日志，不进模型投影） |
| `user` | `turn` `text` | ✓ user | 用户输入 |
| `assistant` | `turn` `text` `reasoning` `toolCalls` `usage` | ✓ assistant | 模型输出 |
| `tool/call` | `turn` `toolCall` | ✗（仅审计） | 工具调用留痕，**含被守卫拦截的调用** |
| `tool/result` | `turn` `toolResult` | ✓ tool | 工具结果回填（按 `callId` 配对） |
| `approval/asked` | `turn` `approval` | ✗（仅审计） | 问了人工 |
| `approval/decided` | `turn` `approval` | ✗（仅审计） | 人工答了（与 asked 同 `id` 配对） |

**投影不变式**：assistant 的 `toolCalls` 与随后的 `tool/result` 按 `callId` 成对——
投给模型的历史始终是合法的（缺 tool 消息会触发服务商报错）。产品层渲染时
也按 `callId` 配对，别按顺序猜。

**目录预算制**：新增一种事件前先删一种。`approval/*` 是 PRD 点名的例外
（§可观测「approval/* 等事件全量落会话日志」、G4「审批挂起-恢复 E2E」），不走预算制。

### 2.3 一个 turn 的事件序列

```
turn/start → user → (assistant → tool/call* → tool/result*)* → assistant → turn/end
```

- `tool/call` 按**模型顺序全部先落**，再执行、再按序回填 `tool/result`（为并发执行留形）。
- 任何出错路径都以 `turn/end{reason:"error"}` 收尾，不留孤儿 turn。
- 人工拒绝**不终止本轮**：模型应停止调工具、改为向用户说明——这需要循环继续一轮。
- 扩展点请求终止本轮时 `turn/end{reason:"terminated"}`。

### 2.4 用量口径

- `usage.prompt_tokens` 是**协议原始值**：OpenAI/Gemini 含缓存命中，Anthropic 不含。
  跨协议比较请用 `fresh_input_tokens`（去缓存后的计费输入）。
- `usage.details.input.cache_read` 是缓存命中（省下来的部分），`cache_write` 是缓存写入。
- 命中率是**自算指标**：`cache_read / (fresh_input + cache_read)`。SDK 侧有
  `Usage.CacheHitRatio()` 可直接用（会话累计用 `Session.TotalUsage()`）。

### 2.5 瞬时事件（transient）不属本契约

流式增量（`RunContext.OnChunk`）**不进事件日志**，是诊断/进度提示。
产品层若要实时显示打字机效果，可以自己转发，但必须遵守：

> **收到增量起临时气泡；收到对应的 `assistant` 事件后，用权威内容替换它。**

否则断线重连补发事件时会出现重复内容或闪烁。transient 事件允许丢弃，
不进任何持久化状态。

---

## 3. 状态序列化契约

### 3.1 会话日志文件

默认后端 `session.JSONL`：每会话一个 `<id>.jsonl` 文件。

```
第 1 行：Header（会话元信息，JSON 对象）
第 2..N 行：事件（每行一个 §2.1 的事件对象）
```

```jsonc
// Header
{
  "id": "s-20261026-1530-ab12cd",
  "createdAt": "2026-10-26T15:30:00Z",
  "title": "修复 lab 里的测试",     // 可选：会话名（空则由产品层兜底）
  "cwd": "/Users/me/proj/lab",     // 可选：工作目录（产品层按它分组）
  "parentId": "",                  // 可选：fork 血缘的父会话 id
  "seedSeq": 0                     // 可选：fork 时复制的日志序号
}
```

**宿主必须先写 Header 再追加事件**：`session.JSONL.Load` 固定跳过首行
（它是 Header），没写 Header 的文件会把第一条事件当成 Header 丢掉。

- Header 写一次（`SaveHeader` 在文件已存在时是 no-op）；事件行只追加。
- 尾部半行（崩溃时最后一条写了一半）在读取时丢弃；**中段损坏直接报错**，
  不静默跳过（日志中段损坏属于异常）。
- 恢复：`session.Restore(id, backend)` 重放事件并**修复孤儿 turn**
  （日志尾部未配对的 `turn/start` → 内存中合成 `turn/end{reason:"interrupted"}`）。
  **只补事件，不回写已有日志**——所以该合成事件不会出现在文件里，
  只在 `Session.Events()` 的内存视图里。产品层若直接读文件会看不到它；
  以 `Session.Events()` 为准。

### 3.2 恢复后的状态

- `Session.Messages()`：事件日志 → 对话消息的**投影**（派生值，随时可重算）。
- `Session.NextTurn()`：下一轮轮次号（恢复后自然接续）。
- `Session.TotalUsage()`：会话累计用量（派生值）。
- 框架配置（模型对象、工具集、策略）**不序列化**——产品层自己持有并重建。

---

## 4. 权威快照 vs transient 事件（硬约束）

> 会话事件日志是**权威状态**；产品层的 UI / 前端只应从中还原渲染状态。
> transient 观测（流式增量、思考过程）是**诊断/进度提示**，不得被 reduce 进
> 权威状态（否则多端并发消费会出现状态分叉）。

落到实现上的三条：

1. **前端 reducer 是纯函数** `events[] → view state`，不持有第二份真相。
2. **断点续传靠 `seq`**：SSE 的 `id:` 就是 `Event.Seq`；客户端重连自带
   `Last-Event-ID`，服务端按 `after=<seq>` 从日志补发。**回放缓冲区就是日志文件本身**，
   不需要内存 ring buffer。
3. **transient 事件不进 reducer**：收到 chunk 起临时气泡，收到对应 `assistant`
   事件后用权威内容替换它（§2.5）。

---

## 5. 并发契约

- **一个 `Session` 同一时刻只允许一个 `Run`**（`Session.Append` 内部加锁仅防误用，
  不承载多写者语义）。
  产品层收到"该会话已有运行"时应**拒绝**（HTTP 409），不要排队、不要并发跑。
- `driver.Agent` 无运行期可变状态，**可被多个 goroutine 并发复用**（不同 Session）。
- `tool.Registry` 线程安全。
- 审批的 `Answerer.Decide` 是**阻塞式**的：驱动线程等一个人工决定。
  取消由 `ctx` 传播（`ctx.Done()` → 归一为 `Cancelled`/`Unavailable`，一律 fail-closed）。
- **批准只对本次调用生效**（allowed-once）：框架每次签发新的审批 id，
  产品层**不得缓存批准**。

---

## 6. 人工审批契约

框架负责编排，问人由宿主注入（`approval.Answerer`）：

```
签发 id → 记「问了」→ 问人 → 归一化结果 → 记「答了」
```

- `approval.Request.ID` 由框架在调用 answerer **之前**签发（进程内唯一 + 时间戳），
  产品层可直接把它当 HTTP 资源 id。
- 四档结果：`approved` / `rejected` / `cancelled` / `unavailable`。
  **`unavailable`（没人应答、通道坏了）一律 fail-closed（拒绝执行）**。
- **未装配 answerer 即拒绝**（NFR-1）：pre-execute 的「需人工确认」降级为拒绝，
  不"先执行再问"。
- 审计写失败就中止（`approval/asked` / `approval/decided` 落会话日志，
  用同一个 id 配对）。
- 拒绝理由回填给模型时，框架已带好"不许绕"的语义（模型该停下解释，
  而不是拆解命令绕过）。

---

## 7. 边界外事项（产品层负责，MoS 不实现）

- HTTP / SSE 服务端点、鉴权、CSRF/DNS-rebinding 防护、会话索引
- 前端渲染、瞬时事件的展示策略、断点续传协议
- 模型密钥的存储与注入方式、provider 选择
- 多租户、任务队列、计费、对象存储
- 文件树 / 终端 / git diff 等具体工作台能力（产品层自行实现，
  但**文件访问应复用 `filesystem.NewSandboxed` 声明的读写根**，别在产品层
  另写一套路径判断——那会绕过 jail）
