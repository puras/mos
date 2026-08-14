# MoS Agent 执行流程

> 来源：`agent/runtime.go` 的 `run` 实现（M1 ~ M2.7 能力全量）。

## 流程图

```mermaid
flowchart TD
    subgraph P["① 准备阶段"]
        A["Run / RunStream(ctx, sess, input)"] --> B{"planner / tools<br/>已配置?"}
        B -->|"否"| E1["返回配置错误"]
        B -->|"是"| C["注入沙箱<br/>WithSandbox(ctx)"]
        C --> D{"配置了 Memory?"}
        D -->|"是"| E["memory.Load 加载历史<br/>→ fromLlm"]
        D -->|"否"| F["sess.History"]
        E --> G["组装系统提示<br/>Assembler 槽位化 / systemPrompt"]
        F --> G
        G --> H["追加用户消息<br/>append → 持久化"]
        H --> I["TaskState 初始化<br/>Goal=input, Status=running"]
    end

    subgraph L["② 任务循环（maxTurns 上限）"]
        I --> J{"turn≥2 且 Compactor?"}
        J -->|"是"| K["压缩历史 → syncHistory<br/>EventCompacted"]
        J -->|"否"| K2["跳过压缩"]
        K --> K2
        K2 --> K3["Assembler 每轮刷新系统提示<br/>替换 history[0]"]
        K3 --> K4{"Retriever 配置?"}
        K4 -->|"是"| K5["RAG 检索 → 知识守门<br/>CustomMsg 注入当轮副本"]
        K4 -->|"否"| K6["toSend = history"]
        K5 --> K6
        K6 --> K7["nextAction<br/>convertToLlm 翻译边界 → 模型调用"]
        K7 --> K8["EventContextUsage 监控<br/>EventStep 思考事件"]
        K8 --> K9{"ToolCall == nil?"}
        K9 -->|"是：直接作答"| DONE["append 回复<br/>Status=Completed → EventDone"]
        DONE --> DONE2["返回回复文本"]
    end

    subgraph T["③ 工具执行链路"]
        K9 -->|"否：调用工具"| T1["append 工具调用消息<br/>EventToolCall"]
        T1 --> T2{"Doom Loop 检测<br/>同工具+同参数连续≥3"}
        T2 -->|"命中"| T3["Status=Blocked<br/>返回错误"]
        T2 -->|"未命中"| T4{"Policy.Allow<br/>安全策略检查"}
        T4 -->|"拒绝"| T5["result=拒绝说明<br/>不执行工具"]
        T4 -->|"放行"| T6["tools.Call<br/>沙箱内执行"]
        T5 --> T7["truncator 截断<br/>结果文本"]
        T6 --> T7
        T7 --> T8["append 工具结果<br/>EventToolResult"]
        T8 --> T9{"Auditor 审计<br/>executed 且无错"}
        T9 -->|"通过"| T10["TaskState.AddFact<br/>EventFact"]
        T9 -->|"未通过"| T11["不写状态"]
        T10 --> J
        T11 --> J
    end

    J -. "循环耗尽" .-> MAX["Status=Blocked<br/>max turns exceeded"]
    MAX --> MAX2["返回错误"]
```

## 阶段说明

### ① 准备阶段

- 校验 `planner` / `tools` 已配置，缺失直接返回错误
- 沙箱注入：`tool.WithSandbox(ctx, a.sandbox)`，shell 类工具从 ctx 取沙箱执行
- 历史加载：配置了 `Memory` 时以 `memory.Load` 为准（`fromLlm` 接回内部层），否则用 `sess.History`
- 系统提示：`Assembler` 槽位化组装优先，否则用静态 `systemPrompt`
- 用户输入追加历史并持久化；`TaskState` 首次运行记录 Goal、进入 running

### ② 任务循环（maxTurns 上限，默认 10）

- 每轮（turn≥2）触发 fresh-context 压缩：切割点不切断 ToolCall-Result 对，摘要取 `TaskState.Snapshot()`，压缩后同步会话历史与持久化记忆
- 每轮刷新系统提示（替换 `history[0]`），运行时状态随 TaskState 进度更新
- 配置了 Retriever 时按 Goal 做 RAG 检索，经知识守门后以 `CustomMsg` 注入**当轮发送副本**（不进历史）
- `nextAction` 先经 `convertToLlm` 翻译边界，再调模型（流式/非流式）；`EventContextUsage` 监控 40% Dumb Zone
- 模型直接作答 → 完成（`EventDone`）；请求工具 → 进入工具链路

### ③ 工具执行链路

- Doom Loop 检测：相同工具 + 相同参数连续 ≥3 次 → 中断置 blocked
- `Policy.Allow` 执行前拦截：拒绝则结果回填拒绝说明（不执行）
- `tools.Call` 沙箱内执行；`truncator` 对模型可见文本截断（Auditor 仍审计原始结果）
- 审计门：仅 `executed && err == nil` 且审计通过的事实写入 `TaskState`（`EventFact`）
- 两个出口：完成（回复文本）与阻塞（doom loop / max turns 耗尽）

## 关键事件序列

`EventStep` → `EventToolCall` → `EventToolResult` → `EventFact` → … → `EventDone`

附加事件：`EventStream`（流式增量）、`EventCompacted`（压缩）、`EventContextUsage`（用量/利用率）、`EventKnowledgeGap`（RAG 知识差距）。
