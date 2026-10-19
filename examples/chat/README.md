# chat：驱动层 ReAct 循环多轮对话 Demo

S4b 里程碑示例——智能体经**驱动层循环**与模型对话，模型可调用内置工具：

- 装配链：llm 协议注册表建模型（套 `llm.WithRetry` + `llm.WithLogging`）→ `planner.NewReAct` 决策器 → `driver.RunContext`（含 `tool.Registry`）→ `react.New()` 驱动
- `MOS_PROVIDER` 切换 openai 兼容 / anthropic / gemini；`llm.WithRetry` 装饰器演示
- `llm.WithLogging` 演示调用观测：每次模型调用在 stderr 打一行（模式 / 耗时 / msgs & tools / finish_reason / token），`MOS_LOG_LLM=0` 关闭；日志套在重试之外，重试几次就见几行
- 内置工具（`builtin.Default()`）登记后模型即可调用；当前可用 `current_time`
- 流式打字机输出（协议不支持时自动回退非流式）；思维链以暗色文本透出
- 会话事件日志落 JSONL（`data/`）：多轮上下文由事件投影累积，**重启自动恢复续聊**；中断的 turn 被合成为 `interrupted` 收尾
- 每轮打印会话累计 token 用量（派生自 assistant 事件）

## 跑法

```bash
cd examples/chat
cp .env.example .env   # 填入 API Key 与模型名
go run .
```

`go run . -new` 开新会话；输入 `q` 退出。

> 工具执行错误（含模型幻觉出的未注册工具名）不中断任务，以显式失败回填给模型
> 让它自行纠正；`RunContext.Tools` 未装配却要求工具时驱动 fail-closed 报错。
> 内置工具目前只有 `current_time`——文件与 shell 类工具等 S5 的 jail 就位后再加入。

## 对应框架能力

| 能力 | 包 |
| --- | --- |
| 统一模型抽象 + 协议注册表 + 装饰器（Retry / Logging） | `llm`、`llm/openai`、`llm/anthropic`、`llm/gemini` |
| 循环驱动接口与运行上下文 | `agent/driver`、`agent/driver/react` |
| 决策器（ReAct） | `planner` |
| 工具接口 / 注册表 / Schema 生成 | `tool`、`tool/builtin` |
| 会话事件日志 / 消息投影 / 持久化 / Restore | `session` |
