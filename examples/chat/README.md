# chat：驱动层 ReAct 循环多轮对话 Demo

S4b 里程碑示例——智能体经**驱动层循环**与模型对话，模型可调用内置工具：

- 装配链：llm 协议注册表建模型（套 `llm.WithRetry` + `llm.WithLogging`）→ `tool.Registry` 登记内置工具 → `agent/policy` 策略装进 `hook.GuardBus` → `planner.NewReAct` 决策器 → `driver.RunContext`（含 `Tools` + `Guards`）→ `react.New()` 驱动
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

## 验证策略拦截（S5c）

策略的判定是**真在跑**的，不是装饰：命中拒绝名单或未列入放行名单的工具不执行，拒绝理由回填给模型。

```bash
go run . -new -deny current_time        # 命令行快捷方式
go run . -new -policy policy.yaml       # 声明式配置（推荐，纯配置装配）
```

启动横幅会打印生效策略及其来源（默认 / 文件名）。问一句「现在几点？」，观察到：

```text
你 > 现在几点？
[guard] 第 1 轮：拒绝 current_time——工具不会执行（工具 "current_time" 在策略拒绝名单中，已拒绝）
[llm #2] stream  0.84s ... finish=tool_calls → 请求调用 current_time
[llm #3] stream  1.20s ... finish=stop ...                 ← 模型读到拒绝理由，改用自己的知识作答
```

对照实验：删掉 `policy.yaml` 里的 `deny` 段（或不传 `-deny`）再问一次，工具会真的执行，
`tool/result` 内容是 `{"iso8601":"2026-10-19T17:36:48+08:00",...}`。

会话日志 `data/chat-*.jsonl` 是唯一事实来源，可直接翻证据：

```bash
grep tool/result data/chat-*.jsonl | tail -1
# 正常：{"kind":"tool/result","toolResult":{"callId":"c1","content":"{\"iso8601\":...}"}}
# 被拒：{"kind":"tool/result","toolResult":{"callId":"c1","content":"工具调用被安全策略拒绝: ...","isError":true}}
```

要点：

- 策略是 **fail-closed** 的：未列入 `allow` 的工具一律拒绝，零值配置 = 拒绝一切。本示例默认放行全部已注册内置工具只是 Demo 取巧（横幅会写明），真平台必须显式声明；
- 被拒的调用**仍有 `tool/call` 审计留痕**，只是 `tool/result` 带 `isError`——审计优先；
- 拒绝不中断 turn，模型下一轮看得见理由并自我纠正；
- `-deny` / `MOS_DENY_TOOLS` 是快捷方式，`-policy` / `MOS_POLICY` 才是平台形态（YAML）；后者启用时前者被忽略。两者都不配时 `Guards` 为 nil，走零开销降级。

## 对应框架能力

| 能力 | 包 |
| --- | --- |
| 统一模型抽象 + 协议注册表 + 装饰器（Retry / Logging） | `llm`、`llm/openai`、`llm/anthropic`、`llm/gemini` |
| 循环驱动接口与运行上下文 | `agent/driver`、`agent/driver/react` |
| 决策器（ReAct） | `planner` |
- 工具接口 / 注册表 / Schema 生成 | `tool`、`tool/builtin` |
| 单调守卫（安全约束） | `agent/hook`、`agent/execpath` |
| 策略层（fail-closed 黑白名单 + YAML） | `agent/policy` |
| 会话事件日志 / 消息投影 / 持久化 / Restore | `session` |
