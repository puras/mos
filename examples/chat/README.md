# chat：直连 LLM 多轮对话 Demo

不经过 agent 循环，直接使用 `llm` 层与模型交互——S2 里程碑的最小可跑示例：

- 协议注册表按 `MOS_PROVIDER` 选适配器（openai 兼容 / anthropic / gemini），`llm.WithRetry` 装饰器演示
- 流式打字机输出（协议不支持时自动回退非流式）；推理思维链以暗色文本透出
- 会话事件日志经 `session` 包落 JSONL（`data/` 目录）：多轮上下文由事件投影累积，**重启自动恢复上次会话继续聊**；中断的 turn 下次启动被合成为 `interrupted` 收尾
- 每轮打印 token 用量

## 跑法

```bash
cd examples/chat
cp .env.example .env   # 填入 API Key 与模型名
go run .
```

`go run . -new` 从全新会话开始；输入 `q` 退出。

## 对应框架能力

| 能力 | 包 |
| --- | --- |
| 统一模型抽象 + 协议注册表 + 装饰器 | `llm`、`llm/openai`、`llm/anthropic`、`llm/gemini` |
| 会话事件日志 / 消息投影 / JSONL 持久化 / Restore | `session` |
