# chat：驱动层 ReAct 循环多轮对话 Demo

S4b 里程碑示例——智能体经**驱动层循环**与模型对话，模型可调用内置工具：

- 装配链：llm 协议注册表建模型（套 `llm.WithRetry` + `llm.WithLogging`）→ `tool.Registry` 登记内置工具 → `agent/policy` 策略装进 `hook.GuardBus` → `planner.NewReAct` 决策器 → `driver.RunContext`（含 `Tools` + `Guards`）→ `react.New()` 驱动
- `MOS_PROVIDER` 切换 openai 兼容 / anthropic / gemini；`llm.WithRetry` 装饰器演示
- `llm.WithLogging` 演示调用观测：每次模型调用在 stderr 打一行（模式 / 耗时 / msgs & tools / finish_reason / token），`MOS_LOG_LLM=0` 关闭；日志套在重试之外，重试几次就见几行
- 内置工具（`builtin.Default()`）登记后模型即可调用；当前可用 `current_time`；文件工具（`read_file`/`write_file`）经 `-workdir` 指定的 jail 根约束后一并登记
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

策略的判定是**真在跑**的，不是装饰。启动横幅会同时列出两份清单：

```text
已注册工具: current_time, read_file, write_file
模型可见工具: read_file, write_file          ← 被策略禁掉的 current_time 消失了
```

**被禁的工具不出现在模型的工具清单里**（可见性过滤，对齐 PI / DSH 的可见性层）——不给模型一个它注定调不成的工具。运行时守卫仍在：模型幻觉出的名字、策略中途变更、非模型路径的直接调用，都会被拦下并留下 `tool/result{isError}`。

```bash
go run . -new -policy policy.yaml       # 声明式配置（推荐，纯配置装配）
go run . -new -deny current_time        # 命令行快捷方式
```

策略语义（对齐 PI / DSH）：**两份名单都可选**，都不写 = 不限制；`allow` 配成空列表 = 全禁；命中 `deny` 的工具从清单里消失。

`policy.yaml` 的形态：

```yaml
deny:
  - current_time
# allow: [read_file, write_file]   # 配了就只剩这些可见
# allow: []                        # 配了空列表 = 全禁
```

**并发读的正解**（同一批多个调用）：`read_file` 声明了可并发，所以让模型读多个文件时它可以一次发多个调用；`next_offset` 指向下一段的起始行号。

会话日志 `data/chat-*.jsonl` 是唯一事实来源：

```bash
grep tool/result data/chat-*.jsonl | tail -1
# {"kind":"tool/result","toolResult":{"callId":"c1","content":"{\"iso8601\":...}"}}
```

要点：

- 策略是**收窄层**：`allow` 未配置 = 不限制，`deny` 命中即拒。fail-closed 的那道门是**注册表**（没注册的工具模型看不见也调不动）——"只允许某几个工具"靠**不注册**其余的来表达；
- 被禁的工具**不在清单里** → 模型不会去试 → 没有被拒记录（审计凭据变成策略配置本身）；若模型仍调了（幻觉/策略中途变更），运行时守卫拦下并留痕；
- 拒绝不中断 turn，模型下一轮看得见理由并自我纠正；
- `-deny` / `MOS_DENY_TOOLS` 是快捷方式，`-policy` / `MOS_POLICY` 才是平台形态（YAML）；后者启用时前者被忽略。两者都不配时不装策略，走零开销降级。

## 对应框架能力

| 能力 | 包 |
| --- | --- |
| 统一模型抽象 + 协议注册表 + 装饰器（Retry / Logging） | `llm`、`llm/openai`、`llm/anthropic`、`llm/gemini` |
| 循环驱动接口与运行上下文 | `agent/driver`、`agent/driver/react` |
| 决策器（ReAct） | `planner` |
- 工具接口 / 注册表 / Schema 生成 | `tool`、`tool/builtin` |
| 单调守卫（安全约束） | `agent/hook`、`agent/execpath` |
| 策略层（fail-closed 黑白名单 + YAML） | `agent/policy` |
| 文件沙箱（Lexical + Realpath 双层校验） | `filesystem`、`tool/builtin` |
| 钩子三环节（裁决 / 环绕 / 结果改写）+ 屏障式并发 | `agent/hook`、`agent/execpath` |
| 进程沙箱（超时 / 进程组 / 输出上限 / 资源限制 / 环境裁剪） | `sandbox`、`tool/builtin` |
| 会话事件日志 / 消息投影 / 持久化 / Restore | `session` |
