# mos 示例

仓库内两个最小可运行示例，配置都走各自目录的 `.env` 文件——
`.env` 在仓库 `.gitignore` 内不入仓；`.env.example` 是占位模板可入仓。

| 示例 | 目录 | 定位 |
|---|---|---|
| 智能体 CLI | `examples/cli` | `assistant.New` + `assistant.Assistant.Run` 一行装配（模型 + 工具 + 安全策略） |
| 协议 CLI | `examples/protocli` | `llm.ChatModel` 统一协议抽象，不经 assistant / 工具层，OpenAI / Anthropic 双协议切换 |
| 学习 CLI | `examples/agentcli` | 七站式教程：在 driver 层逐站装配 `agent/` 运行时（会话事件 → 循环 → 钩子守卫 → 上下文工程 → 审批恢复 → 装配治理），`-stage N` 站站并存，见 [agentcli/README.md](agentcli/README.md) |

## examples/cli：智能体 CLI

接 OpenAI 兼容协议（例：MoGo 网关 `https://go.hegq.net/v1`）。

```bash
cd examples/cli
cp .env.example .env
# 编辑 .env，填入真实的 OPENAI_API_KEY / MOS_BASE_URL / MOS_MODEL
go run .
```

启动时自动加载本目录 `.env`，无需手动 `source`。

- 输入 `q` 退出；工作区是当前目录的 `temp/`（自动创建）
- 工具：时间 / 读 / 写 / shell 四个内置，高风险操作前可挂审批回调

### 切换到流式

```go
out, err := a.RunStream(ctx, sess, line)
```

模型文本增量会实时输出（前提是底层模型支持流式）。

### 加审批

```go
import "github.com/puras/mos/assistant"

confirm := func(ctx context.Context, req assistant.ConfirmRequest) (bool, error) {
    fmt.Fprintf(os.Stderr, "[确认] 工具=%s 原因=%s (y/N): ", req.ToolName, req.Reason)
    var s string
    fmt.Scanln(&s)
    return s == "y" || s == "Y", nil
}

a, _ := assistant.New(model, tools, assistant.Config{
    FileAccess: assistant.FileAccessConfig{WorkspaceRoot: root},
    Confirm:    confirm,
})
```

模型在 shell / write_file 等高风险操作前会停在这里等用户答复。

## examples/protocli：协议 CLI

演示 `llm.ChatModel` 统一抽象：同一份 REPL 代码，经 `-p` 在
OpenAI 兼容协议（DeepSeek / 通义 / GLM / Ollama 等同构）与
Anthropic 原生协议之间切换，Request / Response 在两协议间完全同形。

```bash
cd examples/protocli
cp .env.example .env
# 编辑 .env，填入 OPENAI_* / ANTHROPIC_* 两组配置

go run .                       # 协议取 .env 的 MOS_PROTOCOL，默认 openai
go run . -p anthropic          # 切 Anthropic 原生协议
go run . -p openai -stream     # 流式输出（ChatStream 增量打印）
go run . -p anthropic -think   # 开思考模式（-effort low|medium|high，默认 medium）
```

- 输入 `q` 退出，`/clear` 开新会话（清空多轮历史）
- `/stream`、`/think` 运行中切换流式与思考模式（对下一轮生效）
- 思考模式经统一 `ReasoningEffort` 按协议映射：openai → `reasoning_effort` 参数，
  anthropic → extended thinking（`thinking.budget_tokens`，low/medium/high → 1024/4096/16384）
- 思考内容以暗色输出（`Message.ReasoningContent` / `StreamChunk.Reasoning`），正文正常显示
- 每轮打印 token 用量（in / out / total），验证统一 `llm.Usage` 结构跨协议一致
- 调用失败时撤回该条用户输入，历史不损坏，可直接重试

### Wrap 标准包装器

`llm` 包的标准装饰器（`llm/wrap.go`）可自由组合，本示例用命令行开关装配，
启动横幅展示包装链（内 → 外 = 执行顺序外层先行）：

```bash
go run . -log -timeout 90s -retry 3 -fallback -cache 10m
#   包装: fallback(openai) > timeout(1m30s) > retry(3×2s) > cache(10m0s) > log
```

| 开关 | 包装器 | 行为 |
|---|---|---|
| `-log` | `WithLogging` | stderr 打印每次调用的耗时 / 用量 / 错误 |
| `-retry N -retry-wait d` | `WithRetry` | 非流式失败按固定间隔重试（流式增量已下发不重试） |
| `-timeout d` | `WithTimeout` | 单次调用超时（Chat 与 ChatStream 均生效） |
| `-fallback` | `WithFallback` | 主模型失败切备用（自动取另一协议的 `.env` 配置） |
| `-cache d` | `WithCache` | 非流式结果按请求内容缓存 TTL（流式不缓存） |
