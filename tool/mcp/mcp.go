// Package mcp 把 MCP（Model Context Protocol）服务器的工具接进 MoS 的工具注册表。
//
// 分层：本包是**协议适配层**——把上游 MCP 服务器的工具翻译成 MoS 的
// tool.Tool，并负责连接、发现、换代与失败隔离。框架层不绑配置格式
// （不解析 mcp.json——键名与格式归宿主）、不绑认证（密钥归宿主）、
// 不碰文件系统。
//
// 协议实现：官方 Go SDK（github.com/modelcontextprotocol/go-sdk）。
// 版本协商、分页、schema 直通、结果类型与边界情况都交给 SDK——MoS 不
// 自研 JSON-RPC 层（2026-07-28 规范废弃了 initialize/roots/sampling，
// 手写协议层要同时正确跑两代初始化流程）。SDK 类型不外泄：对外只有
// 本包的 ServerConfig / Client / Tool。
//
// 命名：公开工具名 = mcp__<server>__<tool>（PI / DSH 同款命名空间）。
//
// 暴露三档（对齐 PI 的 exposure，砍掉 codemode——MoS 没有那种形态）：
//   - deferred（默认）：注册但不进模型清单，模型经 tool_search 搜到并
//     激活后才进。MCP 工具面通常很大，全量声明会撑爆 prompt。
//   - direct：注册后直接进模型清单。
//   - hidden：模型看不见、也搜不到；仍可被程序化调用（宿主按名调用）。
package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TransportKind 是 MCP 的传输方式。
type TransportKind string

const (
	// TransportStdio：本地子进程 + stdin/stdout 上的 JSON-RPC。
	TransportStdio TransportKind = "stdio"
	// TransportHTTP：Streamable HTTP（PI 的 streamable-http / DSH 同名）。
	TransportHTTP TransportKind = "streamable-http"
)

// Exposure 决定工具对模型的暴露方式。
type Exposure string

const (
	// ExposureDeferred：注册但不进模型清单；模型经 tool_search 搜到并
	// 激活后，下一轮才进清单。
	ExposureDeferred Exposure = "deferred"
	// ExposureDirect：注册后直接进模型清单。
	ExposureDirect Exposure = "direct"
	// ExposureHidden：模型看不见、也搜不到（不进 deferred 集）；
	// 仍可被程序化调用（宿主或别的工具按名调用）。
	ExposureHidden Exposure = "hidden"
)

// ParseExposure 解析暴露档字符串。未知值退到 deferred（最保守的一档：
// 模型看不见，但可被搜到激活）。
func ParseExposure(s string) Exposure {
	switch Exposure(strings.ToLower(strings.TrimSpace(s))) {
	case ExposureDirect:
		return ExposureDirect
	case ExposureHidden:
		return ExposureHidden
	default:
		return ExposureDeferred
	}
}

// ServerConfig 是一个 MCP 服务器的配置。
//
// 零值可用：Transport 空 = stdio；Exposure 空 = deferred；超时取 Options
// 里的默认值。**不解析任何配置文件**——键名与文件格式归宿主（治理规则 5：
// 框架不绑配置格式）。
type ServerConfig struct {
	// Name 是服务器名（命名空间）：公开工具名 = mcp__<name>__<tool>。
	// 必填；同一份配置里重名 = 装配错误。
	Name string
	// Description 是人类可读描述（宿主展示用；**也进模型提示词**——
	// 有 deferred 工具的服务器会进静态系统提示词的 mcp_servers 节，
	// S8.4-A）。
	Description string
	// Transport 是传输方式（空 = stdio）。
	Transport TransportKind

	// ── stdio ──
	// Command 是要启动的子进程（可执行文件路径）。
	Command string
	// Args 是子进程参数。
	Args []string
	// Env 是追加的环境变量（同名覆盖；其余继承宿主环境）。
	Env map[string]string
	// Dir 是子进程工作目录（空 = 继承）。
	Dir string

	// ── streamable http ──
	// URL 是服务端点。
	URL string
	// Headers 是附加请求头（**每个请求都带上**）：宿主已取好的 token、
	// 自定义租户标识等都归这里。OAuth 不在本包职责内。
	Headers map[string]string
	// TokenProvider 返回当前访问凭据（Bearer token）。
	//
	// 契约：框架**每次请求取一次**，**不缓存、不刷新**；返回空串 = 不注入
	// Authorization 头；返回 error = 该请求失败（归认证类）。
	// 只对 streamable-http 生效（stdio 是本地子进程，不走 HTTP）。
	// 详见 TokenProvider 的完整契约。
	TokenProvider TokenProvider

	// ── 通用 ──
	// Exposure 是服务器级默认暴露档（空 = deferred）。
	Exposure Exposure
	// ToolExposure 是**逐工具覆盖**：键是上游原始工具名，精确名优先于
	// "*"（PI 的 getMcpToolExposure 同款语义）。
	ToolExposure map[string]Exposure
	// Disabled = true 时不连接、不注册（配置留着不启用）。
	Disabled bool
}

// ToolInfo 是上游服务器给的一个工具的原始描述。
type ToolInfo struct {
	// Name 是上游工具名（原始名；公开名由 ToolName 拼）。
	Name string
	// Description 是上游描述（模型看到的描述用它）。
	Description string
	// InputSchema 是上游的 JSON Schema（**原样透传**——MoS 没有对应结构体，
	// 反推等于瞎猜）。
	InputSchema any
}

// Result 是 MCP 工具结果在 MoS 侧的投影。
//
// 投影口径：文本直接给模型；非文本内容（图像 / 音频 / 资源）降级成
// **占位诊断**（MoS 的模型看不了图，DSH 走附件系统、PI 落临时文件——
// 两条路 MoS 都没有），原始 JSON 留在 Opaque 里供程序化调用方取。
type Result struct {
	// Content 是给模型看的文本（上游 text 内容拼接）。
	Content string `json:"content"`
	// IsError 是上游标注的失败（上游把错误放进 content 并置 isError）。
	IsError bool `json:"isError"`
	// Structured 是上游的结构化输出（原样）。
	Structured any `json:"structured,omitempty"`
	// Opaque 是非文本内容的占位与原始 JSON。
	Opaque []OpaquePart `json:"opaque,omitempty"`
}

// OpaquePart 是**非文本**内容（图像 / 音频 / 资源 / 资源链接）的占位。
type OpaquePart struct {
	// Type 是内容类型（image / audio / resource / resource_link / unknown）。
	Type string `json:"type"`
	// Note 是给模型看的诊断（为什么这部分没有内容）。
	Note string `json:"note"`
	// JSON 是原始 JSON（截断过——二进制动辄几百 KB）。
	JSON string `json:"json,omitempty"`
}

// Client 是一个已连接的 MCP 服务器会话的最小接口。
//
// 框架层只认这个接口：宿主可以自己实现它（测试替身、远程网关代理……），
// 不必碰 SDK 类型。
type Client interface {
	// ListTools 列出服务器的全部工具（自动翻页）。
	ListTools(ctx context.Context) ([]ToolInfo, error)
	// CallTool 调用一个工具（name 是**上游原始名**）。
	CallTool(ctx context.Context, name string, args map[string]any) (*Result, error)
	// Instructions 是服务器给的说明（连接建立时一次性注入给模型）。
	Instructions() string
	// ProtocolVersion 是协商后的协议版本（快照与诊断用）。
	ProtocolVersion() string
	// Close 关闭连接。
	Close() error
}

// ErrorKind 区分 MCP 失败的层次：协议语义错误 vs 传输层故障。
type ErrorKind string

const (
	// ErrorProtocol：协议层/上游语义错误（工具报错、响应不合法）。
	ErrorProtocol ErrorKind = "protocol"
	// ErrorTransport：连接、超时、取消——连接不可用。
	ErrorTransport ErrorKind = "transport"
	// ErrorAuth：认证失败——HTTP 401/403，或取凭据失败。
	//
	// 单独分一类：处置不同。传输层错误是「这台服务器挂了，稍后重试」，
	// 认证错误是「这台服务器要凭据，宿主得先把 token 给对」——后者
	// 重试多少次都一样。
	ErrorAuth ErrorKind = "auth"
)

// Error 是 MCP 侧的错误（带服务器与工具归属，便于快照与日志定位）。
type Error struct {
	// Kind 是失败层次（protocol / transport / auth）。
	Kind ErrorKind
	// Server 是服务器名。
	Server string
	// Tool 是上游工具名（连接级错误时为空）。
	Tool string
	// Err 是底层错误。
	Err error
}

func (e *Error) Error() string {
	who := e.Server
	if e.Tool != "" {
		who = e.Server + "/" + e.Tool
	}
	if who == "" {
		return fmt.Sprintf("mcp %s: %v", e.Kind, e.Err)
	}
	return fmt.Sprintf("mcp %s: %s: %v", e.Kind, who, e.Err)
}

// Unwrap 让 errors.Is / errors.As 能看到底层错误。
func (e *Error) Unwrap() error { return e.Err }

// Options 是 MCP 装配参数（Manager 与 Dial 共用）。
type Options struct {
	// Servers 是要连接的服务器列表。
	Servers []ServerConfig
	// ClientName 是向服务器报出的客户端名（空 = "mos"）。
	ClientName string
	// ClientVersion 是向服务器报出的客户端版本（空 = "dev"）。
	ClientVersion string
	// ConnectTimeout 是单个服务器的连接超时（0 = DefaultConnectTimeout）。
	ConnectTimeout time.Duration
	// CallTimeout 是单次工具调用超时（0 = DefaultCallTimeout）。
	CallTimeout time.Duration
	// OnToolsChanged 是「服务器的工具列表变了」回调（server = 服务器名）。
	//
	// 契约：**不许阻塞**——SDK 在自己的读循环里调它，阻塞会把协议层卡住。
	// 实现应只做「投递信号」，实际刷新交给别的 goroutine。
	OnToolsChanged func(server string)
	// Dial 是连接服务器的工厂（nil = 用本包的 Dial）。
	//
	// 存在意义：宿主可以换掉连接方式（走自己的网关代理、注入凭据、复用
	// 长连接），测试也能用替身顶掉真连接。
	Dial func(ctx context.Context, cfg ServerConfig, opts Options) (Client, error)
}

const (
	// DefaultConnectTimeout 是默认连接超时（15s）。
	//
	// 取值理由：连接失败要**尽快**暴露（装配期就该知道这台服务器不可用），
	// 但 stdio 服务器冷启动可能要拉依赖，给 15s 余地；超时后进入失败隔离
	// （该服务器不注册工具，其余照常）。
	DefaultConnectTimeout = 15 * time.Second
	// DefaultCallTimeout 是默认调用超时（60s，PI / DSH 同款）。
	DefaultCallTimeout = 60 * time.Second
	// DefaultMaxInstructionBytes 是服务器说明注入的字节上限（32KB，DSH 同款）。
	DefaultMaxInstructionBytes = 32 * 1024
)

// HiddenSlot 是 MCP hidden 档在工具注册表可见性过滤器栈里的槽名。
//
// 具名槽：policy 热替换只动 policy 那层，不会把这一层清掉。
const HiddenSlot = "mcp-hidden"
