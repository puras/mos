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
- `-deny` / `MOS_DENY_TOOLS` 是快捷方式，`-policy` / `MOS_POLICY` 才是平台形态（YAML）；后者启用时前者被忽略。两者都不配时 `Guards` 为 nil，走零开销降级；
- 读策略文件与解析 YAML 都是**应用层**的活（`os.ReadFile` → `yaml.Decoder` 严格解析 → `policy.Config`）：框架只认结构，不碰文件系统、不绑定配置格式——否则等于绕过 S5d 的文件 jail。键名由应用自己的 `appPolicy` DTO 定，与框架解耦。

## 验证文件沙箱（S5d）

文件工具的路径先经 **Lexical + Realpath 双层校验**，越界路径的工具**不执行**，拒绝理由回填给模型。

```bash
go run . -new -workdir .          # jail 根 = 当前目录（默认）
go run . -new -workdir /tmp/demo   # 或指定别的根
```

启动横幅会打印 `文件沙箱: /path/to/cwd（越界路径会被拒绝，工具不会执行）`。

问一句「读一下 ../../etc/passwd」，可观察到：

```text
你 > 读一下 ../../etc/passwd
[llm #2] stream  ... finish=tool_calls → 请求调用 read_file
[llm #3] stream  ... finish=stop ...    ← 模型读到拒绝理由后改口
```

`tool/result` 里是拒绝理由（`路径 "../../etc/passwd" 不在允许的根内（read 操作允许的根=[...]）`），带 `isError:true`；同一次调用的 `tool/call` 仍留痕（审计优先）。

对照实验：`go run . -new -workdir /tmp/demo` 后让模型写一个文件，它会写到 `/tmp/demo` 下；换成 `-workdir .` 再写 `../escape.txt` 就会被拒。

要点：

- **读也走 Realpath**：jail 里一个指向 `/etc/passwd` 的 symlink 同样读不出来（只做词法校验的话就能读出 jail）；
- **不装就是不开**：不装配 `filesystem` 就不该注册文件工具——不给模型这个能力，比给它一个必然失败的工具干净；
- 沙箱根由宿主（示例的 `-workdir`）决定，框架不替宿主选路径，也不预设 Local/SaaS 形态：示例声明的是 `{ReadRoots: [根], WriteRoots: [根]}`，宿主想放开读或写就自己加根（写 `"/"` 即完全放开，且这是一条看得见的规则）。

## 验证钩子与并发（S5e）

钩子（HookBus）与守卫（GuardBus）是**两条轨道**：守卫单调裁决（拒绝/放行），钩子可改写（裁剪结果、环绕派发、裁决调用）。

```bash
go run . -new                       # 装好钩子（观测 + 计时）
go run . -new -trim 200             # 追加结果裁剪（post-execute 钩子）
go run . -new -concurrency 1        # 强制串行（对照）
```

启动横幅会打印并发上限。运行时 stderr 可见钩子的三个环节：

```text
[hook] 第 1 轮：即将执行 read_file        ← pre-execute（裁决环节）
[hook] read_file 执行完毕（1ms）          ← tool/execute（环绕派发）
```

`-trim 200` 时，`tool/result` 里能看到被裁剪的文本（`…（已裁剪，原 N 字节）`）——这是 post-execute 钩子真正改了回填给模型的内容。

### 并发的规则

- **只有声明可并发的工具才并发**：工具实现 `tool.ConcurrencySafe(args) bool` 并返回 true 才入并行组；未声明 / 参数非法 / 分类器出错 → **独占**（串行）。本示例里 `read_file` 可并发，`write_file` 独占。
- **独占调用形成屏障**：`[read(A), read(B), write(A), read(C)]` → `[read(A) ∥ read(B)]` → `[write(A)]` → `[read(C)]`。
- **回填顺序永远是模型的调用序**，与完成顺序无关（`tool/result` 按 `CallID` 一一对回）。
- 只有工具体并发；preflight（守卫裁决 + pre-execute）与回填（post-execute）都按模型序串行。

### 载荷冻结

钩子**改不了**工具名 / 参数 / CallID——改写即违约，该调用被拒绝并说明原因。所以钩子无法绕过策略守卫：守卫判定的就是这次调用的最终载荷，且守卫只跑一次。

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
| 会话事件日志 / 消息投影 / 持久化 / Restore | `session` |
