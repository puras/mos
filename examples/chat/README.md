# chat：驱动层 ReAct 循环多轮对话 Demo

S4a 里程碑示例——智能体经**驱动层循环**与模型对话（不再直连模型）：

- 装配链：llm 协议注册表建模型 → `planner.NewReAct` 决策器 → `driver.RunContext` → `react.New()` 驱动
- `MOS_PROVIDER` 切换 openai 兼容 / anthropic / gemini；`llm.WithRetry` 装饰器演示
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

> 当前驱动为 S4a 无工具形态：若模型请求调用工具，驱动会 fail-closed 报错
> （工具执行通道在 S4b 接入），不会静默忽略。

## 对应框架能力

| 能力 | 包 |
| --- | --- |
| 统一模型抽象 + 协议注册表 + 装饰器 | `llm`、`llm/openai`、`llm/anthropic`、`llm/gemini` |
| 循环驱动接口与运行上下文 | `agent/driver`、`agent/driver/react` |
| 决策器（ReAct） | `planner` |
| 会话事件日志 / 消息投影 / 持久化 / Restore | `session` |
