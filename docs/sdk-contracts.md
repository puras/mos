# MoS SDK 契约（面向 SaaS 智能体平台集成）

> 目的：明确 MoS 作为**智能体内核 SDK** 提供给上层平台的稳定契约，划定职责边界，指导平台集成。

## 1. 职责边界

| 层 | 归属 | 职责 |
| --- | --- | --- |
| MoS | SDK（本仓库） | Agent 任务循环、工具系统、记忆、审计门、任务状态、上下文工程、事件观测 |
| SaaS 平台 | 独立新平台 | HTTP/WS API、多租户、身份鉴权、任务调度、计费、观测 Dashboard、对象存储、人工确认通道 |
| MoGo | 模型接入层 | 密钥管理、模型路由、协议归一、限流、计费 |

**MoS 明确不做**：HTTP 服务、多租户隔离、鉴权、任务队列、计费、可视化、对象存储。平台通过 SDK 注入与事件流自行承担。

## 2. SDK 契约清单

| 契约 | 状态 | 说明 |
| --- | --- | --- |
| 状态可恢复（Session/TaskState 序列化 + Restore） | ✅ 已实现 | `Session.Save()` / `agent.RestoreSession()`，JSON 契约见 §3 |
| 并发模型 | 📖 文档约定 | 一个 `Session` 同一时刻只允许一个 `Run`；`Agent` 无状态可并发复用（见 §4） |
| 事件契约 | 📖 文档约定 | `Observer` 事件流可 JSON 序列化，是平台观测对接的稳定契约（见 §5） |
| 人工介入 | 🔧 待完善 | `Confirmer` 当前为同步接口；异步确认（webhook/轮询）抽象待扩展（见 §6） |
| 配置注入 | ✅ 已具备 | `assistant.Config` 结构化注入；`SystemAssembler` 按会话组装 |
| 后端可替换 | ✅ 已具备 | `memory.Memory` / `Retriever` / `Sandbox` 均为接口注入，平台可实现自己的后端 |
| 模型接入 | ✅ 已具备 | `llm.ChatModel` 模型对象注入；走 MoGo 网关时用 OpenAI 兼容适配器 |
| 流式输出 | ✅ 已具备 | `RunStream` 事件流，平台可直接转 SSE |

## 3. 状态序列化契约（Session / TaskState）

### 序列化入口

```go
// 平台持久化：存库 / 对象存储
data, err := sess.Save()

// 平台恢复：跨进程 / 重启后还原会话
sess, err := agent.RestoreSession(data)
```

### JSON 结构

```jsonc
{
  "id": "session-001",
  "history": [
    {
      "role": "user",                 // system | user | assistant | tool | ""
      "content": "请计算 1+2",
      "content_parts": [],            // 多模态内容块，可选
      "tool_calls": [{"id": "", "name": "calculator", "args": "{\"expr\":\"1+2\"}"}],
      "tool_call_id": "",             // 仅 tool 消息
      "name": "",                     // 仅 tool 消息
      "reasoning_content": "",        // 思维链，须原样回传
      "exclude_from_context": false,  // MoS 运行期元数据
      "summary": false,               // M2.5 压缩摘要标记
      "source": "user"                // user/assistant/tool/system/summary/custom
    }
  ],
  "state": {
    "goal": "请计算 1+2",
    "facts": [{"summary": "", "evidence": "", "tool": "calculator"}],
    "pending": [],
    "blocked": [],
    "status": "running"               // pending | running | blocked | completed
  }
}
```

### 契约要点

- `status` 序列化为可读字符串（`pending/running/blocked/completed`），反序列化兼容字符串与数字
- `history` 完整保留两层消息（含 `ExcludeFromContext` / `Summary` / `Source` 元数据），恢复后可无缝续跑
- `Save()` 只序列化数据；**Agent 配置（模型对象/工具集/策略）不序列化**——平台自行持有配置并在恢复时重建 Agent
- 恢复后续跑：`a.Run(ctx, restoredSess, input)`，`run` 自动将非 running 状态置为 running 并追加新输入

## 4. 并发契约

- `Agent` 无运行期可变状态，**可被多个 goroutine 并发复用**（不同 Session）
- 一个 `Session` 同一时刻**只允许一个 `Run`/`RunStream` 写入**（History / TaskState 无内部锁）；平台需保证 per-session 串行（如按 SessionID 分片到单一 worker）
- `tool.Registry` 线程安全；`Observer` 回调在 Run 的调用 goroutine 内同步触发，平台若需跨 goroutine 转发应自行排队

## 5. 事件契约（Observer）

```go
type Observer func(Event)
```

- 事件序列：`EventStep` → `EventToolCall` → `EventToolResult` → `EventFact` → … → `EventDone`
- 附加事件：`EventStream`（流式增量）、`EventCompacted`（压缩）、`EventContextUsage`（用量/利用率）、`EventKnowledgeGap`（RAG 知识差距）
- `Event` 全部字段可 JSON 序列化（`ToolResult any` 为具体工具返回值），平台可整体落盘做轨迹回放
- `Usage` 字段承载 token 用量，是平台计费 / 成本核算的素材（走 MoGo 网关时计费在网关侧）
- 平台集成建议：`Observer` 内只做"入队转发"，不阻塞 Run 循环

## 6. 人工介入（待完善）

- 现状：`assistant.Confirmer` 为同步进程内接口（y/N），适用于 CLI 场景
- 平台需求：Web 化异步确认（发起确认 → 平台弹窗/通知 → 用户回填 → 继续执行）
- 待办：SDK 扩展异步确认抽象（返回 pending 确认 token，平台异步回填后驱动 Policy 放行），当前平台可先以同步接口 + 进程内回调自行封装

## 7. 平台集成指南（最小闭环）

1. **持久化**：每次 `Run` 结束后调用 `sess.Save()` 存库（按 `SessionID`）
2. **恢复**：收到续跑请求时 `agent.RestoreSession(data)` 还原，重建 Agent 后 `Run` 续跑
3. **观测**：`WithObserver` 将事件 JSON 化后写入平台的日志/指标管道
4. **隔离**：每个任务注入独立 `WorkDir` + 沙箱；多租户数据隔离由平台在存储层保证
5. **模型**：经 MoGo 网关统一接入（OpenAI 兼容 + `MOS_BASE_URL` 语义），MoS 侧无需感知多协议

## 8. 边界外事项（平台负责，MoS 不实现）

- HTTP/WS 服务端点、SSE 协议封装、Webhook 回调
- 租户 / 用户 / 项目模型与数据隔离
- API Key / JWT 鉴权与 RBAC
- 任务队列、限流、配额、优先级调度
- 计费结算（token 用量由 MoS `Usage` 事件提供素材）
- 文件上传 API 与对象存储（文件读取在 MoS 侧复用 `read_file` / RAG）
