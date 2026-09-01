# mos CLI 示例

一个最小可运行 CLI，展示如何用 `assistant.New` + `assistant.Assistant.Run` 集成 mos。

接 OpenAI 兼容协议（例：MoGo 网关 `https://go.hegq.net/v1`）。配置走 `.env` 文件——
`.env` 在仓库 `.gitignore` 内不入仓；`.env.example` 是占位模板可入仓。

## 准备

```bash
cd examples/cli
cp .env.example .env
# 编辑 .env，填入真实的 OPENAI_API_KEY / MOS_BASE_URL / MOS_MODEL
```

## 跑

```bash
go run .
```

启动时自动加载本目录 `.env`，无需手动 `source`。

输入 `q` 退出。

工作区是当前目录的 `temp/`（自动创建）。

## 切换到流式

```go
out, err := a.RunStream(ctx, sess, line)
```

模型文本增量会实时输出（前提是底层模型支持流式）。

## 加审批

```go
import "hegq.net/mos/assistant"

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

## 切回 Anthropic / Gemini

改 `main.go` 里的 import 与 `New(...)` 调用：

```go
import "hegq.net/mos/llm/anthropic"

model, _ := anthropic.New(llm.Config{
    "api_key":  os.Getenv("ANTHROPIC_API_KEY"),
    "base_url": "https://api.anthropic.com",
    "model":    "claude-3-5-sonnet-latest",
    "version":  "2023-06-01",
})
```

`.env` 也对应换成 `ANTHROPIC_API_KEY=...`。