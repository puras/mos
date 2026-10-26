# chat：驱动层 ReAct 循环多轮对话 Demo

S4b 里程碑示例——智能体经**驱动层循环**与模型对话，模型可调用内置工具：

- 装配链：llm 协议注册表建模型（套 `llm.WithRetry` + `llm.WithLogging`）→ `tool.Registry` 登记内置工具 → `agent/policy` 策略装进 `hook.GuardBus` → `planner.NewReAct` 决策器 → `driver.RunContext`（含 `Tools` + `Guards`）→ `react.New()` 驱动
- `MOS_PROVIDER` 切换 openai 兼容 / anthropic / gemini；`llm.WithRetry` 装饰器演示
- **重试策略是宿主的**（`-retry-attempts` / `-retry-delay`，默认 3 次 / 1 秒基数）：只重试**瞬时**错误（限流 429、5xx、连接失败、空响应），4xx、鉴权失败、用户取消一律**快速失败**——重试确定性的错误只是浪费配额。退避是**有界指数 + 抖动**；流式只在还没吐出内容时才重试
- `llm.WithLogging` 演示调用观测：每次模型调用在 stderr 打一行（模式 / 耗时 / msgs & tools / finish_reason / token），`MOS_LOG_LLM=0` 关闭；日志套在重试之外，重试几次就见几行
- **重试也要看得见**：每次重试前在 stderr 打一行 `[llm] 第 N 次重试（…后）：<错误>`——静默重试是查不出问题的
- 内置工具（`builtin.Default()`）登记后模型即可调用；当前可用 `current_time`；文件工具（`read_file`/`write_file`）经 `-workdir` 指定的 jail 根约束后一并登记
- **shell 工具默认不装配**（`-allow-shell` 开启）：给 agent 一条验证回路——改完代码能自己跑一次 `go build` / `go test`，而不是靠反复重写去猜。命令经 `sandbox.Local` 执行（超时、进程组击杀、输出上限、资源限制、环境裁剪）
- **路径强制**（macOS）：写限定在工作区内，越界写被**内核**拒绝（`Operation not permitted`）——与命令怎么拼无关，拆解命令绕不过。被隔离命令的缓存/临时目录由宿主指到工作区内（`GOCACHE`/`TMPDIR`）；非 macOS 平台此强制未落地，非全开调用会明确报错
- 开了 shell 会附带一份**默认系统引导词**（教模型用命令验证）；`MOS_SYSTEM_PROMPT` 可覆盖
- **写前观察**（S5h）：`write_file` 覆盖没读过的文件、或读后文件已被修改（shell/人改的）→ **拒绝并要求重读**——防丢失更新。新建文件不需要先读
- **人工审批**（`approval.go`）：pre-execute 的「需人工确认」会在终端问 `[审批] 是否允许执行 shell？[y/N]`——**空回车 = 拒绝**（默认答案必须是安全的那个）。未装配时（`-approval=false`）一律拒绝（fail-closed）。批准只对**本次调用**生效，不缓存
- **宿主侧参数级规则**（`guard.go`）：按参数判定——敏感文件（`.env` / `id_rsa` / `*.pem`）不读不写（deny），**删除命令**（`rm` / `unlink` / `git rm`，无论带什么 flag）需人工确认（ask）。**规则键在语义上，不在字母形状上**（曾键在"含 r 的 flag"上：`git status --short` 误触发、逐个 `git rm` 漏过，被拒后模型拆解命令绕过了拦截）。诚实边界：`find -delete`、脚本语言仍绕得过——这只是业务规则，真正的边界是路径强制隔离（S5f-2b，未落地）
- `lab/` 是配套的**练手项目**（独立的 go module，故意留了个逻辑 bug）：让 agent 修好它并跑测试——仓库自己的 `go test ./...` 不受它影响（嵌套 module 被根 module 忽略）
- 流式打字机输出（协议不支持时自动回退非流式）；思维链以暗色文本透出
- 会话事件日志落 JSONL（`data/`）：多轮上下文由事件投影累积，**重启自动恢复续聊**；中断的 turn 被合成为 `interrupted` 收尾
- 每轮打印会话累计 token 用量：**输入（去缓存口径）/ 缓存读 / 缓存写 / 输出 / 共**——缓存命中单列才看得出成本结构（Anthropic 的输入本就不含缓存，OpenAI 的含，故统一用去缓存口径）

## 跑法

```bash
cd examples/chat
cp .env.example .env   # 填入 API Key 与模型名
go run .
```

`go run . -new` 开新会话；输入 `q` 退出。

## 验证 shell 验证回路（S5f-1）

```bash
go run . -new -allow-shell                       # 开启 shell（默认不装配）
go run . -new -allow-shell -shell-timeout 300    # 放宽缺省超时（秒）
go run . -new -allow-shell -shell-max-timeout 600  # 放宽超时上界（秒，模型申请更久的天花板）
go run . -new -allow-shell -shell-spill /tmp/mos  # 输出落盘目录（截断时全文写这里）
```

开启后模型多了 `shell` 工具，于是这条回路成立：**改 → 跑 → 看真实输出 → 再改**。

### 走一遍：`lab/` 练手项目

`lab/` 是一个独立的 go module，故意留了一个逻辑 bug（`SumRange` 少算 `hi` 那一项）。问一句：

```text
你 > lab 里有个测试没过，修好它并跑测试验证
```

期望看到的事件流（下面是**真实跑出来的**，非模拟）：

```text
tool/call   read_file {"path":".../lab/main.go","offset":1,"limit":30}
tool/result ok        {"content":"// Package main 是 chat 示例的练手项目…"}
tool/call   read_file {"path":".../lab/main_test.go","offset":1,"limit":30}
tool/result ok        {"content":"package main\n\nimport \"testing\"…"}
tool/call   shell     {"command":"cd lab && go test ./... "}
tool/result ok        {"exit_code":1,"output":"--- FAIL: TestSumRange (0.00s)
                          main_test.go:7: SumRange(1, 5) = 10, want 15（含两端）
                          [exit code: 1]",...}
tool/call   edit_file {"path":".../lab/main.go","edits":[{"old_string":"for i := lo; i < hi; i++ {","new_string":"for i := lo; i <= hi; i++ {"}]}
tool/result ok        {"bytes":484,"first_changed_line":12,...}
tool/call   shell     {"command":"cd lab && go test ./... "}
tool/result ok        {"exit_code":0,"output":"ok  \tmoslab\t0.501s\n","duration_ms":1008,"truncated":false}
```

关键在**中间那两条**：模型先跑了一次测试、**看到了真实失败**，才去改；改完再跑一次、通过。这条回路是 agent 自主收敛的前提——没有它，模型只能靠猜代码对不对。

注意 `go test` 在 `lab/` 里跑得起来，靠的是沙箱保留了 `PATH` 与 `HOME`（`GOMODCACHE` 在 HOME 下）；环境变量是**白名单**，主机的密钥不会泄给模型执行的命令。

### 沙箱做了什么（详见 `docs/ARCHITECTURE.md` §3.7）

| 层 | 挡住什么 |
| --- | --- |
| 工作目录（宿主固定） | 相对路径的默认落点落在工作区外 |
| 超时 + 进程组击杀 | 死循环、fork 出的孤儿进程 |
| 输出上限 64KB | 输出爆炸撑爆内存 |
| 资源限制（ulimit） | 吃满 CPU / 内存 / 磁盘 |
| 环境白名单 | 把主机密钥泄给模型执行的命令 |
| 剥离代理变量 | 尽力而为，挡不住直连 |

要点：

- **超时的缺省与上界都由宿主定**（两个旋钮）：`-shell-timeout` 是缺省（默认 120s），`-shell-max-timeout` 是上界（默认 300s）。模型可以在调用里要求更短，**要求更长会被夹到上界**——超时是资源预算，预算的宽窄由宿主定，不由模型定；
- **路径范围不受限**（诚实边界）：命令仍能访问运行用户权限内的任意路径——Go 标准库给不了强制隔离，需要 bwrap / Landlock / Seatbelt / 容器后端（`sandbox.Runner` 是留给它们的接缝）；
- **输出截断有逃生通道**：完整输出落盘（`-shell-spill` 指定目录），回执给模型 `output_path`，它可以 `read_file` 读全文；
- **非零退出码是事实不是故障**：命令跑了但没成功 → **正常结果**（回执带 `exit_code`，输出末尾有 `[exit code: N]` 标记）；只有"命令根本没起来"（超时/被拒）才报错。对齐 DSH——这么分是因为把非零退出当失败会逼模型自己写 hack 绕开（它会拼 `; echo "exit=$?"` 让整条命令以 0 退出）；
- **默认不装配**：不给模型这个能力，比给它一个必然失败的工具干净。

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
