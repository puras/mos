# LLM 统一抽象层（llm）

> 来源：`llm/` 整个包 —— `model.go`（核心类型 + ChatModel 接口 + Capability 位标志）+ `registry.go`（协议注册表）+ `wrap.go`（5 个标准装饰器：Logging/Retry/Timeout/Fallback/Cache）。
> 不在本文档：`llm/openai/` / `llm/anthropic/` / `llm/gemini/` / `llm/transport/` —— 厂商适配器专文（`docs/llm-adapters.md`，Task #13）。
> 目标读者：理解"MoS 怎么把多家 LLM 抽象成一个统一接口"的工程师；想写自定义模型接入或横切包装器的开发者。

---

## 1. 一句话定位

**`llm` 是 MoS 的"统一 LLM 抽象层"**：定义三个核心类型（`Message` / `Request` / `Response` + `ToolCall` / `Tool` / `Usage` / `StreamChunk`）和一个 `ChatModel` 接口（`Chat` / `ChatStream` / `Capabilities`），用 **Capability 位标志** 描述协议差异，用 **`Registry`** 让平台按名注册厂商适配器（OpenAI / Anthropic / Gemini / ...），用 **5 个标准装饰器**（Logging / Retry / Timeout / Fallback / Cache）做横切能力 —— **统一协议使横切逻辑可插拔，组合顺序由调用方显式书写**。

源码顶端的注释（model.go:1-3 + wrap.go:11-18）讲清核心哲学：

> "**统一协议（ChatModel 接口）使横切逻辑可以包装为可插拔的标准装饰器**：任何包装器都实现 ChatModel 接口并组合底层 ChatModel，调用方按需嵌套组合。**约定：签名统一为 WithXxx(m ChatModel, ...) ChatModel；组合顺序即执行顺序（外层先执行）。**与'通用中间件链'的区别：这里**没有隐式顺序与全局注册表**，每个包装器职责单一、嵌套关系由调用方显式书写，可读性与可测试性更好。"

文件分布：

| 文件 | 行数级 | 主题 |
|---|---|---|
| `model.go` | ~170 行 | Capability 位 + Role + Message/ToolCall/Tool/Usage/Request/Response/StreamChunk + ChatModel 接口 |
| `registry.go` | ~60 行 | Config + Factory + Registry（按名创建 ChatModel） |
| `wrap.go` | ~210 行 | 5 个 WithXxx 装饰器（Logging/Retry/Timeout/Fallback/Cache） |

---

## 2. 整体协作位置

```
平台装配（registry）
  ├─ llm.Register(reg, "openai", func(cfg) llm.ChatModel { return openai.NewChatModel(cfg) })
  ├─ llm.Register(reg, "anthropic", func(cfg) llm.ChatModel { return anthropic.NewChatModel(cfg) })
  └─ ...
      ↓ model, _ := reg.Create("openai", cfg)
      ↓ model = llm.WithLogging(model, logf)
      ↓ model = llm.WithRetry(model, 3, 200*time.Millisecond)
      ↓ model = llm.WithTimeout(model, 60*time.Second)
      ↓ model = llm.WithFallback(model, backupModel)
  ↓
  *llm.ChatModel（组装好的横切能力链）
      ↓
  driver.Run → model.Chat / model.ChatStream
```

**关键设计**：

- **Registry 只管协议适配**（model.go 注释）："注册表只做名字到工厂的映射，不感知协议差异"
- **Wrap 只管横切能力**（wrap.go 注释）："注册表产出裸模型，横切能力（日志/重试/超时等）由调用方按需包装"
- **职责分离**让 Registry / Wrap 完全独立，组合由调用方显式书写

---

## 3. `ChatModel` 接口（model.go:153-164）

```go
type ChatModel interface {
    Chat(ctx context.Context, req *Request) (*Response, error)
    ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error)
    Capabilities() Capability
}
```

**三个方法**：

| 方法 | 职责 |
|---|---|
| `Chat` | 发起一次非流式对话，返回完整 Response |
| `ChatStream` | 发起流式对话，增量块通过 emit 推送；返回值为聚合后的完整响应 |
| `Capabilities` | 返回模型支持的能力位（function calling / streaming / ...） |

**关键设计**（注释 model.go:154-155）：

> "仅包含对话能力（Chat/ChatStream）；**后续其他能力（如 Embedding/Image）以并列接口（EmbeddingModel 等）扩展**，需要全能力时再组合。"

—— 不让 ChatModel 接口膨胀（避免所有厂商实现都要 stub 无关方法）。

**`emit` 返回 error 可中断输出**（注释 model.go:159-160）—— 流式调用方可在 emit 内做 ctx 检查 / 用户取消。

---

## 4. `Capability` 位标志（model.go:9-23）

```go
type Capability uint32

const (
    CapFunctionCalling  Capability = 1 << iota  // 支持函数调用（工具）
    CapStreaming                                // 支持流式输出
    CapStructuredOutput                         // 支持结构化输出（response_format）
    CapMultimodal                               // 支持多模态输入（图片/音频内容块）
    CapReasoning                                // 支持推理模型参数（reasoning_effort）+ 推理 token 统计
)
```

**`HasCapability` 判断**（model.go:167-169）：

```go
func (c Capability) HasCapability(cap Capability) bool {
    return c&cap != 0
}
```

**平台用法**：

```go
caps := model.Capabilities()
if caps.HasCapability(llm.CapReasoning) {
    req.ReasoningEffort = ptr("medium")  // 只在支持时设置
}
if !caps.HasCapability(llm.CapStructuredOutput) {
    return nil, llm.ErrUnsupported      // 不支持时报错（fail-closed）
}
```

**关键设计**：协议差异用**位标志**而非类型断言/子类区分 —— 适配器无需导出"我是 Anthropic"标识，上层用能力组合判断。

---

## 5. 核心类型（model.go）

### 5.1 `Message`（model.go:59-69）

```go
type Message struct {
    Role             Role          `json:"role"`
    Content          string        `json:"content,omitempty"`
    ContentParts     []ContentPart `json:"content_parts,omitempty"`  // 多模态，优先于 Content
    ToolCalls        []ToolCall    `json:"tool_calls,omitempty"`     // 仅 assistant
    ToolCallID       string        `json:"tool_call_id,omitempty"`   // 仅 tool，关联 ToolCalls
    Name             string        `json:"name,omitempty"`           // 仅 tool，工具名
    ReasoningContent string        `json:"reasoning_content,omitempty"` // 推理内容（思维链）
}
```

**两条不变量**：

1. **`ContentParts` 优先于 `Content`**（注释 model.go:43-44）：
   > "Content 是纯文本快捷方式；当 ContentParts 非空时，适配器应优先将其展开为协议的多模态格式"
2. **`ReasoningContent` 多轮必须回传**（注释 model.go:67-68）：
   > "仅 assistant 消息；thinking 模式多轮对话须原样回传，否则服务端报错"
   —— 这是 DeepSeek/Qwen 等推理模型的关键约束。

### 5.2 `ContentPart` 多模态（model.go:45-56）

```go
type ContentPart struct {
    Type string `json:"type"`           // text | image | audio | file
    Text string `json:"text,omitempty"`
    URL  string `json:"url,omitempty"`  // image/audio 远程地址
    Data string `json:"data,omitempty"` // image/audio base64
    MIME string `json:"mime_type,omitempty"`
}
```

—— 一个类型承载所有模态（text/image/audio/file），由 `Type` 字段决定序列化形态。

### 5.3 `ToolCall`（model.go:36-40）

```go
type ToolCall struct {
    ID   string `json:"id"`
    Name string `json:"name"`
    Args string `json:"args"`  // JSON 编码的参数（字符串，不是对象）
}
```

**`Args` 是字符串**而非 `map[string]any` —— 避免厂商适配层反复 marshal/unmarshal。**driver 层拿到 Args 后再 unmarshal 到结构体**（如 `tool.Parse[requestHumanParams](json.RawMessage(args))`）。

### 5.4 `Tool`（model.go:72-76）

```go
type Tool struct {
    Name        string `json:"name"`
    Description string `json:"description"`
    Parameters  any    `json:"parameters"`  // JSON Schema 对象（map[string]any 或结构体）
}
```

—— `Parameters` 是任意 JSON Schema 对象，适配器自己负责序列化。

### 5.5 `Usage` + `UsageDetails`（model.go:78-109）

```go
type Usage struct {
    PromptTokens     int           `json:"prompt_tokens"`
    CompletionTokens int           `json:"completion_tokens"`
    TotalTokens      int           `json:"total_tokens"`
    Details          UsageDetails  `json:"details,omitempty"`
}

type UsageDetails struct {
    Input  InputTokenDetails  `json:"input,omitempty"`
    Output OutputTokenDetails `json:"output,omitempty"`
}

type InputTokenDetails struct {
    CacheRead  int `json:"cache_read,omitempty"`   // 缓存命中
    CacheWrite int `json:"cache_write,omitempty"`  // 缓存写入
    Audio      int `json:"audio,omitempty"`
}

type OutputTokenDetails struct {
    Reasoning int `json:"reasoning,omitempty"`  // 推理 token
    Audio     int `json:"audio,omitempty"`
}
```

**核心字段**（PromptTokens / CompletionTokens / TotalTokens）三厂商都有；**Details 是可选明细**（缓存命中/写入、推理 token），缺失时为零值 —— 让不支持的厂商能优雅降级。

### 5.6 `Request`（model.go:121-133）

```go
type Request struct {
    Messages         []Message       `json:"messages"`
    Tools            []Tool          `json:"tools,omitempty"`
    Temperature      *float64        `json:"temperature,omitempty"`       // 指针：nil = 用厂商默认
    MaxTokens        *int            `json:"max_tokens,omitempty"`         // 指针：nil = 不限制
    ResponseFormat   *ResponseFormat `json:"response_format,omitempty"`   // 结构化输出
    Modalities       []string        `json:"modalities,omitempty"`        // text | audio | image
    ReasoningEffort  *string         `json:"reasoning_effort,omitempty"`  // low | medium | high
}
```

**全部用指针**：让"未设置"和"设置为 0"区分（0 在 temperature/max_tokens 都是有意义的）。

**`ResponseFormat` 类型**（model.go:112-119）：

```go
type ResponseFormat struct {
    Type   string `json:"type"`              // json_schema | json_object | text
    Name   string `json:"name,omitempty"`
    Schema any    `json:"schema,omitempty"`  // JSON Schema 对象
}
```

### 5.7 `Response` + `StreamChunk`（model.go:135-151）

```go
type Response struct {
    Message      Message `json:"message"`
    Usage        Usage   `json:"usage"`
    FinishReason string  `json:"finish_reason,omitempty"`  // stop/length/tool_calls/end_turn/max_tokens/STOP/MAX_TOKENS
}

type StreamChunk struct {
    Delta        string     // 增量文本
    Reasoning    string     // 推理内容增量
    ToolCall     *ToolCall  // 工具调用增量（部分厂商把 tool call 拆成多个 chunk 拼出）
    FinishReason string
}
```

**`FinishReason` 跨厂商归一化**（注释 model.go:139-141）：

> "openai: stop/length/tool_calls；anthropic: end_turn/max_tokens/tool_use；gemini: STOP/MAX_TOKENS/... **已转小写**。length/max_tokens 表示输出被截断，**循环层据此对未执行完的工具调用做 fail-closed 处理**（见 agent/driver/react.ReActDriver）"

—— 适配器把 Gemini 的 `STOP` 转为小写，循环层用归一化值判断。

---

## 6. `Registry` 协议注册表（registry.go）

### 6.1 一句话定位

**`Registry` 是 MoS 的"L1 注册表"**（与 `agent/seam` 是 L2 注册表配套）：**装配期**按名注册 `Factory`，运行期 `Create(name, cfg)` 创建 `ChatModel`。**注册表只做名字到工厂的映射，不感知协议差异**。

注释（registry.go:9-13）：

> "装配方按名注册 Factory、按配置创建 ChatModel；**注册表只做名字到工厂的映射，不感知协议差异**。**与 wrap.go 的装饰器分工：注册表产出裸模型，横切能力（日志/重试/超时等）由调用方按需包装**。"

### 6.2 数据结构

```go
type Config map[string]string   // 通用配置：api_key / base_url / model / version 等

type Factory func(cfg Config) (ChatModel, error)

type Registry struct {
    mu        sync.RWMutex
    factories map[string]Factory
}
```

### 6.3 四个方法

| 方法 | 职责 |
|---|---|
| `NewRegistry()` | 创建空注册表 |
| `Register(name, f)` | 注册 Factory（后注册覆盖先注册） |
| `Create(name, cfg)` | 按名创建 ChatModel |
| `Names()` | 返回已注册协议名（**排序后**） |

### 6.4 `Register` 覆盖语义

```go
func (r *Registry) Register(name string, f Factory) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.factories[name] = f    // 后注册覆盖先注册（与 agent/seam 一致）
}
```

—— 平台可以用同名字覆盖默认实现。

### 6.5 `Create` 错误信息友好

```go
func (r *Registry) Create(name string, cfg Config) (ChatModel, error) {
    r.mu.RLock()
    f, ok := r.factories[name]
    r.mu.RUnlock()
    if !ok {
        return nil, fmt.Errorf("llm: unknown provider %q, registered: %v", name, r.Names())
    }
    return f(cfg)
}
```

—— 未注册时报错带 `registered: [...]` 列出已注册项，便于排查。

### 6.6 与 `agent/seam` 的关系

| 维度 | `llm.Registry` | `agent/seam.SeamRegistry` |
|---|---|---|
| 注册对象 | LLM 协议（OpenAI/Anthropic/...） | 框架组件（driver/model/tool/policy/...） |
| 输出类型 | `llm.ChatModel` | `any`（按接口断言） |
| 注入位 | LLM 接入层（platform/internal/config） | Config 装配层 |
| 后注册覆盖 | ✅ | ✅ |

两者是不同层级的注册表，**互不引用**：llm 给 LLM 用、seam 给框架组件用。注释（registry.go:11-13）明确分工：

> "与 wrap.go 的装饰器分工：注册表产出裸模型，横切能力（日志/重试/超时等）由调用方按需包装"

---

## 7. 五个 WithXxx 标准装饰器（wrap.go）

### 7.1 统一签名

```go
type ChatModel
func WithLogging(m ChatModel, logf func(CallResult)) ChatModel
func WithRetry(m ChatModel, attempts int, delay time.Duration) ChatModel
func WithTimeout(m ChatModel, timeout time.Duration) ChatModel
func WithFallback(m ChatModel, backup ChatModel) ChatModel
func WithCache(m ChatModel, ttl time.Duration) ChatModel
```

**组合顺序即执行顺序（外层先执行）**：

```go
model = llm.WithLogging(model, logf)             // 1. 最外层：日志
model = llm.WithFallback(model, backupModel)     // 2. 主失败切备用
model = llm.WithRetry(model, 3, 200*time.Ms)     // 3. 主失败重试
model = llm.WithTimeout(model, 60*time.Second)   // 4. 内层：超时（最先生效）
m := openai.NewChatModel(cfg)                    // 5. 最底层：实际厂商
```

注释（wrap.go:14-18）：

> "**组合顺序即执行顺序（外层先执行）**。与'通用中间件链'的区别：这里**没有隐式顺序与全局注册表**，每个包装器职责单一、嵌套关系由调用方显式书写，**可读性与可测试性更好**。"

—— 这与 Express/Koa 的中间件模型本质区别：MoS 不维护"全局中间件链"，调用方在每处自己嵌套。

### 7.2 `WithLogging` 观测包装器（wrap.go:31-54）

```go
type CallResult struct {
    Req     *Request
    Resp    *Response
    Err     error
    Elapsed time.Duration
    Stream  bool
}

func WithLogging(m ChatModel, logf func(CallResult)) ChatModel
```

**关键设计**：

- **统一在结束时上报**（注释 wrap.go:30）：发起/结果/失败都在 Chat / ChatStream 返回时回调（不在中途）
- **`Stream` 字段区分**流式 vs 非流式
- **日志内容（模型名/地址等）由调用方闭包持有**（注释 wrap.go:30-31）—— 本包装器只负责时机与上下文

### 7.3 `WithRetry` 重试包装器（wrap.go:58-95）

```go
func WithRetry(m ChatModel, attempts int, delay time.Duration) ChatModel
```

**三条不变量**：

| 不变量 | 实现 | 含义 |
|---|---|---|
| **`attempts <= 0` 视为 1** | `if attempts <= 0 { attempts = 1 }` | 默认值安全 |
| **ChatStream 不重试** | 直接透传 | **增量已下发，重试会重复输出** |
| **ctx 取消立即返回** | `case <-ctx.Done()` | 重试可中断 |

注释（wrap.go:57-58）：

> "**ChatStream 不重试（增量已下发，重试会重复输出），直接透传**"

—— 这是 Streaming 与 Chat 在重试语义上的关键差异。

### 7.4 `WithTimeout` 超时包装器（wrap.go:97-119）

```go
func WithTimeout(m ChatModel, timeout time.Duration) ChatModel
```

**最简实现**：`context.WithTimeout(ctx, timeout)` + `defer cancel()`，对 Chat 与 ChatStream 都生效。

**嵌套行为**：

```go
model = WithTimeout(WithTimeout(m, 10s), 5s)   // 内层 5s 生效（先到先 cancel）
```

—— 多层嵌套时内层超时先生效，但外层 cancel 会传播。

### 7.5 `WithFallback` 备用模型包装器（wrap.go:121-153）

```go
func WithFallback(m ChatModel, backup ChatModel) ChatModel
```

**关键设计**：

- **主模型失败时切备用**（`err != nil`）
- **`backup == nil` 等价直接透传**（注释 wrap.go:122-123）
- **`Capabilities` 取并集**（wrap.go:132-137）：
  ```go
  if w.backup == nil { return w.m.Capabilities() }
  return w.m.Capabilities() | w.backup.Capabilities()
  ```
  —— 即使主模型不支持某能力，备用模型支持时仍然声明支持（让上层能用到）

### 7.6 `WithCache` 缓存包装器（wrap.go:155-213）

```go
func WithCache(m ChatModel, ttl time.Duration) ChatModel
```

**三条不变量**：

| 不变量 | 实现 | 含义 |
|---|---|---|
| **缓存键 = 请求规范化 JSON 的 sha256** | `cacheKey(req)` (wrap.go:209-213) | 同请求同响应 |
| **ChatStream 不缓存** | 直接透传 | 流式无完整 Request/Response |
| **`ttl <= 0` 不过期** | `if w.ttl > 0 && time.Now().After(e.expiry)` | 永久缓存（慎用） |

**`cacheKey` 实现**（wrap.go:209-213）：

```go
func cacheKey(req *Request) string {
    b, _ := json.Marshal(req)        // 序列化整个 Request（含 Messages/Tools）
    sum := sha256.Sum256(b)
    return string(sum[:])
}
```

—— 完整 Request 哈希（含所有 Messages + Tools），任何字段变化都重新生成 key。

**缓存响应共享只读**（注释 wrap.go:156）：

> "缓存的响应为共享只读对象，**调用方不应修改**"

—— 因为返回的 `*Response` 是同一份，调用方改它会污染缓存。

---

## 8. `ErrUnsupported` 与错误模型（model.go:171-173）

```go
var ErrUnsupported = errors.New("llm: capability not supported")
```

**适配器用法**：

```go
if !caps.HasCapability(llm.CapStructuredOutput) && req.ResponseFormat != nil {
    return nil, llm.ErrUnsupported
}
```

**调用方用法**：

```go
resp, err := model.Chat(ctx, req)
if errors.Is(err, llm.ErrUnsupported) {
    // 切换到支持结构化输出的模型
}
```

—— 单一 sentinel 错误让上层做"能力降级路由"。

---

## 9. 关键不变量

| 不变量 | 实现位置 | 含义 |
|---|---|---|
| **ChatModel 不膨胀** | 仅 Chat/ChatStream/Capabilities | Embedding 等并列扩展 |
| **Capability 位标志** | model.go:9-23 | 协议差异用位组合 |
| **未设置 ≠ 0** | 指针字段（Temperature/MaxTokens） | 区分"用厂商默认" |
| **ReasoningContent 多轮回传** | Message 注释 | 服务端约束 |
| **`ContentParts` 优先 `Content`** | Message 注释 | 多模态优先 |
| **Args 是字符串** | ToolCall.Args | 避免反复 marshal |
| **FinishReason 跨厂商归一化** | Response.FinishReason 注释 | 适配器转小写 |
| **Details 可选** | Usage.Details | 不支持厂商优雅降级 |
| **Registry 只管协议** | registry.go 注释 | 不做横切能力 |
| **后注册覆盖先注册** | Registry.Register | 平台可覆盖默认 |
| **装饰器外层先执行** | WithXxx 嵌套顺序 | 显式组合 |
| **ChatStream 不重试不缓存** | WithRetry/WithCache | 增量重复风险 |
| **`attempts <= 0` = 1** | WithRetry | 默认安全 |
| **Capabilities 取并集** | WithFallback | 主不支持时备用支持仍声明 |
| **缓存响应共享只读** | WithCache 注释 | 防污染 |
| **`ctx` 取消传播** | WithRetry/WithTimeout | 可中断 |

---

## 10. 与 driver / assistant 的协作

```
assistant.Config{Model: "claude-sonnet-4-5"}
   ↓ config.AssembleConfig
   ↓ llm.Registry.Create("claude-sonnet-4-5", cfg)
   ↓ anthropic.NewChatModel(cfg)
   ↓ anthropic.ChatModel（实现 llm.ChatModel + Capabilities）
   ↓ assistant.New 内部按需嵌套 WithLogging/WithRetry/...
   ↓
   *llm.ChatModel（带横切能力的最终模型）
   ↓
   driver.Run → model.Chat(req) / model.ChatStream(req, emit)
   ↓
   resp, err := model.Chat(ctx, req)
   ↓ driver 解析 resp.Message.ToolCalls
   ↓ execpath.ExecuteBatch(...)
```

**关键衔接点**：

| 层 | 衔接 |
|---|---|
| `assistant.Config.Model` | 传给 `llm.Registry.Create(name, cfg)` |
| `assistant.New` 内部 | （可选）给 model 嵌套 WithLogging 等装饰器 |
| `driver.Agent.Run` | 调 `rt.Model.Chat / ChatStream` |
| `react.ReActDriver` | 读 `resp.FinishReason` 判断是否截断 |
| `driver.execpath.ExecuteHook` | 解析 `ToolCall.Args` 字符串 |

**注意**：当前 assistant.New 不内置 WithXxx 装饰器（注释暗示横切能力由业务侧按需包装）—— 平台集成时自行嵌套。

---

## 11. 自定义扩展姿势

### 11.1 注册自定义厂商

```go
reg := llm.NewRegistry()
llm.Register(reg, "my-custom", func(cfg llm.Config) (llm.ChatModel, error) {
    apiKey := cfg["api_key"]
    baseURL := cfg["base_url"]
    return myCustomChatModel.New(apiKey, baseURL), nil
})

model, err := reg.Create("my-custom", llm.Config{
    "api_key": "...",
    "base_url": "https://api.example.com",
    "model": "my-model-v1",
})
```

### 11.2 嵌套标准装饰器

```go
m, _ := reg.Create("openai", cfg)

// 嵌套顺序（外层先执行）
m = llm.WithLogging(m, func(r llm.CallResult) {
    log.Printf("[llm] elapsed=%v err=%v stream=%v", r.Elapsed, r.Err, r.Stream)
})
m = llm.WithFallback(m, backupModel)             // 主失败切备用
m = llm.WithRetry(m, 3, 200*time.Millisecond)   // 非流式 3 次重试
m = llm.WithTimeout(m, 60*time.Second)          // 单次 60s 超时

// 用法（与无装饰器完全一致）
resp, err := m.Chat(ctx, &llm.Request{
    Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
})
```

### 11.3 自定义装饰器

```go
func WithRateLimit(m llm.ChatModel, rps int) llm.ChatModel {
    limiter := rate.NewLimiter(rate.Limit(rps), rps)
    return &rateLimitWrapper{m: m, limiter: limiter}
}

type rateLimitWrapper struct {
    m       llm.ChatModel
    limiter *rate.Limiter
}

func (w *rateLimitWrapper) Capabilities() llm.Capability { return w.m.Capabilities() }
func (w *rateLimitWrapper) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
    if err := w.limiter.Wait(ctx); err != nil { return nil, err }
    return w.m.Chat(ctx, req)
}
func (w *rateLimitWrapper) ChatStream(ctx context.Context, req *llm.Request, emit func(llm.StreamChunk) error) (*llm.Response, error) {
    if err := w.limiter.Wait(ctx); err != nil { return nil, err }
    return w.m.ChatStream(ctx, req, emit)
}
```

### 11.4 能力位驱动的请求构造

```go
caps := model.Capabilities()
req := &llm.Request{Messages: msgs}

if caps.HasCapability(llm.CapFunctionCalling) {
    req.Tools = tools
}
if caps.HasCapability(llm.CapStructuredOutput) {
    req.ResponseFormat = &llm.ResponseFormat{Type: "json_schema", Schema: mySchema}
}
if caps.HasCapability(llm.CapReasoning) {
    effort := "medium"
    req.ReasoningEffort = &effort
}
```

### 11.5 完整接入示例（OpenAI + 4 个横切能力）

```go
reg := llm.NewRegistry()
llm.Register(reg, "openai", func(cfg llm.Config) (llm.ChatModel, error) {
    return openai.NewChatModel(cfg), nil
})

base, _ := reg.Create("openai", llm.Config{
    "api_key": os.Getenv("OPENAI_API_KEY"),
    "model":   "gpt-4o",
    "base_url": "https://api.openai.com/v1",
})

backup, _ := reg.Create("anthropic", llm.Config{
    "api_key": os.Getenv("ANTHROPIC_API_KEY"),
    "model":   "claude-sonnet-4-5",
})

model := llm.WithLogging(base, func(r llm.CallResult) {
    metrics.RecordLLM(r.Elapsed, r.Err)
})
model = llm.WithFallback(model, backup)
model = llm.WithRetry(model, 3, 500*time.Millisecond)
model = llm.WithTimeout(model, 90*time.Second)
model = llm.WithCache(model, 5*time.Minute)   // 命中相同的请求 5 分钟内复用

// 用法
resp, err := model.Chat(ctx, &llm.Request{
    Messages: []llm.Message{
        {Role: llm.RoleSystem, Content: "You are a helpful assistant."},
        {Role: llm.RoleUser, Content: "Hello"},
    },
})
```

---

## 12. 一句话总结

**`llm` 是 MoS 的"统一 LLM 抽象层"：三个核心类型（Message/Request/Response）+ ToolCall/Tool/Usage/StreamChunk + ChatModel 接口（Chat/ChatStream/Capabilities）+ Capability 位标志（FunctionCalling/Streaming/StructuredOutput/Multimodal/Reasoning）+ Registry 按名注册厂商适配器 + 5 个标准装饰器（Logging/Retry/Timeout/Fallback/Cache）。协议差异用 Capability 位组合表达、横切能力用 WithXxx 装饰器显式组合（外层先执行）、流式与缓存/重试语义区分（增量重复风险）、ReasoningContent 多轮必须回传、FinishReason 跨厂商归一化 —— 让任何 LLM 厂商都能 100-200 行接入、任何横切能力都能 30 行包装。** 是 MoS 抽象层的最核心一层，让上层 driver/agent 完全不知道"我在跟谁对话"。