// Package transport 提供 LLM 适配器共享的 HTTP envelope 与用量归一化。
//
// 三件套：
//   - Client：POST + JSON body + 协议鉴权头的共享请求骨架，Endpoint(stream)
//     支持流式/非流式端点切换（Gemini 需要）；
//   - ParseStatusError：非 200 响应的统一错误转换（provider 前缀 + 协议错误体
//     解析回调），内部读完并关闭 Body（幂等，调用方 defer 二次关闭安全）；
//   - UsageFrom：moproto *usage.Usage → llm.Usage 的字段归一
//     （ReadInputTokens/CachedTokens、ThinkingTokens/ReasoningTokens 互斥非零，
//     取 max 无损）。
//
// 客户端解析与顶层 httpx capability seam 的关系：Client.Do 未显式配置
// http_timeout 时每次请求经 httpx.FromContextStream(ctx) 解析——平台
// WithClient 一处注入（统一代理/重试/指标），工具层与 LLM 调用两端生效；
// 显式配置 http_timeout 时使用构造期私有客户端（显式配置优先于注入；
// 运行时改共享 client 的 Timeout 属数据竞态，只能构造期私有化）。
//
// 注意：注入的客户端若自带硬超时（http.Client.Timeout 覆盖整个响应体读取），
// 超过该时长的流式输出会被截断，注入方需自行保证流式安全。
package transport
