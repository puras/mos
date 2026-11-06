package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/puras/mos/httpx"
)

// maxOpaqueJSON 是非文本内容保留的原始 JSON 字节上限。
//
// 为什么要截：图像是 base64，几百 KB 很常见；整段塞进工具结果会把上下文
// 撑爆（且 MoS 的模型看不了图——留个诊断就够了）。原始 JSON 截断保留，
// 程序化调用方需要完整内容时自己走 MCP 协议调。
const maxOpaqueJSON = 2048

// Dial 连接一个 MCP 服务器（按配置选 transport），返回已连接的客户端。
//
// 客户端能力：**显式置空**（&sdk.ClientCapabilities{}）——SDK 默认会声明
// roots 能力，而 MoS 不实现 roots（框架不碰宿主的文件系统边界，
// 治理规则 5）。「能力即证据」：不声明就不用，服务器的 roots 请求
// 也就不会来。
func Dial(ctx context.Context, cfg ServerConfig, opts Options) (Client, error) {
	if cfg.Name == "" {
		return nil, errors.New("mcp: 服务器名不能为空")
	}
	transport, err := buildTransport(ctx, cfg)
	if err != nil {
		return nil, &Error{Kind: ErrorTransport, Server: cfg.Name, Err: err}
	}
	connectCtx := ctx
	if opts.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(ctx, opts.ConnectTimeout)
		defer cancel()
	}
	name := opts.ClientName
	if name == "" {
		name = "mos"
	}
	version := opts.ClientVersion
	if version == "" {
		version = "dev"
	}
	client := sdk.NewClient(
		&sdk.Implementation{Name: name, Version: version},
		&sdk.ClientOptions{
			// 空能力 = 不声明 roots / sampling / elicitation（MoS 都不实现）。
			Capabilities: &sdk.ClientCapabilities{},
			ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) {
				// 回调在 SDK 的读循环里被调用 —— 绝不能阻塞。
				// 实现方只投递信号，实际刷新在别的 goroutine 里做。
				if opts.OnToolsChanged != nil {
					opts.OnToolsChanged(cfg.Name)
				}
			},
		},
	)
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, wrapError(cfg.Name, err)
	}
	c := &sdkClient{server: cfg.Name, session: session, callTimeout: opts.CallTimeout}
	if init := session.InitializeResult(); init != nil {
		c.protocol = init.ProtocolVersion
		c.instructions = init.Instructions
	}
	return c, nil
}

// buildTransport 按配置构造传输层。
func buildTransport(ctx context.Context, cfg ServerConfig) (sdk.Transport, error) {
	switch cfg.Transport {
	case "", TransportStdio:
		if cfg.Command == "" {
			return nil, errors.New("stdio 传输需要 Command")
		}
		cmd := exec.Command(cfg.Command, cfg.Args...)
		cmd.Dir = cfg.Dir
		// 子进程的 stderr 直接进宿主 stderr：MCP 服务器的日志/崩溃栈要看得到
		//（不接管道——没人读的话管道会写满，卡死子进程）。
		cmd.Stderr = os.Stderr
		if len(cfg.Env) > 0 {
			env := os.Environ()
			for k, v := range cfg.Env {
				env = append(env, k+"="+v)
			}
			cmd.Env = env
		}
		return &sdk.CommandTransport{Command: cmd}, nil
	case TransportHTTP:
		if cfg.URL == "" {
			return nil, errors.New("streamable-http 传输需要 URL")
		}
		hc := httpx.FromContext(ctx)
		// 有静态头或凭据提供器 → 套一层（克隆客户端，不改宿主传进来的）。
		if len(cfg.Headers) > 0 || cfg.TokenProvider != nil {
			hc = withAuth(hc, cfg)
		}
		return &sdk.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: hc,
		}, nil
	default:
		return nil, errors.New("未知传输方式 " + string(cfg.Transport))
	}
}

// sdkClient 是 SDK 会话的 MoS 侧封装。
type sdkClient struct {
	server       string
	session      *sdk.ClientSession
	callTimeout  time.Duration
	instructions string
	protocol     string
}

// ListTools 列出全部工具（SDK 自动翻页）。
func (c *sdkClient) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var out []ToolInfo
	for t, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, &Error{Kind: classify(err), Server: c.server, Err: err}
		}
		if t == nil {
			continue
		}
		out = append(out, ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return out, nil
}

// CallTool 调用一个上游工具并投影结果。
func (c *sdkClient) CallTool(ctx context.Context, name string, args map[string]any) (*Result, error) {
	if c.callTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.callTimeout)
		defer cancel()
	}
	res, err := c.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, &Error{Kind: classify(err), Server: c.server, Tool: name, Err: err}
	}
	return project(res), nil
}

// ListResources 列出全部资源（SDK 迭代器自动翻页），投影成 MoS 类型。
func (c *sdkClient) ListResources(ctx context.Context) ([]ResourceInfo, error) {
	var out []ResourceInfo
	for r, err := range c.session.Resources(ctx, nil) {
		if err != nil {
			return nil, &Error{Kind: classify(err), Server: c.server, Err: err}
		}
		if r == nil {
			continue
		}
		out = append(out, ResourceInfo{
			URI:         r.URI,
			Name:        r.Name,
			Description: r.Description,
			MIMEType:    r.MIMEType,
			Size:        r.Size,
		})
	}
	return out, nil
}

// ListResourceTemplates 列出全部资源模板（SDK 迭代器自动翻页）。
func (c *sdkClient) ListResourceTemplates(ctx context.Context) ([]ResourceTemplateInfo, error) {
	var out []ResourceTemplateInfo
	for rt, err := range c.session.ResourceTemplates(ctx, nil) {
		if err != nil {
			return nil, &Error{Kind: classify(err), Server: c.server, Err: err}
		}
		if rt == nil {
			continue
		}
		out = append(out, ResourceTemplateInfo{
			URITemplate: rt.URITemplate,
			Name:        rt.Name,
			Description: rt.Description,
			MIMEType:    rt.MIMEType,
		})
	}
	return out, nil
}

// ReadResource 读取一个资源，把内容段投影成 MoS 类型。
//
// 判别靠 Blob 是否为 nil（SDK 的 ResourceContents 是单结构体双字段）。
func (c *sdkClient) ReadResource(ctx context.Context, uri string) ([]ResourceContents, error) {
	if c.callTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.callTimeout)
		defer cancel()
	}
	res, err := c.session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, &Error{Kind: classify(err), Server: c.server, Err: err}
	}
	out := make([]ResourceContents, 0, len(res.Contents))
	for _, rc := range res.Contents {
		if rc == nil {
			continue
		}
		out = append(out, ResourceContents{
			URI:      rc.URI,
			MIMEType: rc.MIMEType,
			Text:     rc.Text,
			Blob:     rc.Blob,
		})
	}
	return out, nil
}

// Instructions 返回服务器说明。
func (c *sdkClient) Instructions() string { return c.instructions }

// ProtocolVersion 返回协商后的协议版本。
func (c *sdkClient) ProtocolVersion() string { return c.protocol }

// Close 关闭会话。
func (c *sdkClient) Close() error { return c.session.Close() }

// classify 区分传输层故障、认证失败与协议语义错误。
//
// 超时与取消是**连接层**的事（连接不可用 / 上下文结束），连接断开（EOF /
// 管道关闭）同样归传输层；**HTTP 401/403 与取凭据失败归认证类**；其余按
// 协议错误归类——三类错误在快照与日志里的处置不同：传输层意味着「这台
// 服务器现在不可用」，认证层意味着「这台服务器要凭据」，协议层意味着
// 「这次调用被上游拒绝」。
func classify(err error) ErrorKind {
	// 认证错误已经被结构化（authTransport 直接造 *Error）——优先认它。
	var mcpErr *Error
	if errors.As(err, &mcpErr) && mcpErr.Kind == ErrorAuth {
		return ErrorAuth
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrorTransport
	}
	// 连接断开：子进程退出 / HTTP 流断 / 传输管道关闭。
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return ErrorTransport
	}
	return ErrorProtocol
}

// wrapError 给错误补上服务器归属，并**保留内层的结构化分类**。
//
// 认证错误必须保住：SDK 会把 RoundTripper 的错误层层包装（url.Error →
// SDK 内部错误），若一律包成 transport，「需要认证」这条诊断就丢了。
func wrapError(server string, err error) error {
	var mcpErr *Error
	if errors.As(err, &mcpErr) {
		if mcpErr.Server == "" {
			mcpErr.Server = server
		}
		return mcpErr
	}
	return &Error{Kind: classify(err), Server: server, Err: err}
}

// IsAuthError 判断错误是不是认证类（宿主据此提示「这台服务器要凭据」）。
func IsAuthError(err error) bool {
	var mcpErr *Error
	return errors.As(err, &mcpErr) && mcpErr.Kind == ErrorAuth
}

// project 把上游结果投影成 MoS 的 Result。
//
// 文本直接给模型；图像 / 音频 / 资源降级成占位诊断（MoS 没有图片入模、
// 不做 MCP 资源工具——两条路都没有），原始 JSON 截断保留。
func project(res *sdk.CallToolResult) *Result {
	out := &Result{IsError: res.IsError, Structured: res.StructuredContent}
	var texts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdk.TextContent:
			texts = append(texts, v.Text)
		case *sdk.ImageContent:
			out.Opaque = append(out.Opaque, opaquePart("image", "图像内容未进模型上下文（MoS 不支持图片入模）", v))
		case *sdk.AudioContent:
			out.Opaque = append(out.Opaque, opaquePart("audio", "音频内容未进模型上下文（MoS 不支持音频入模）", v))
		case *sdk.ResourceLink:
			out.Opaque = append(out.Opaque, opaquePart("resource_link", "资源链接未展开（v1 不做 MCP 资源工具）", v))
		case *sdk.EmbeddedResource:
			out.Opaque = append(out.Opaque, opaquePart("resource", "内嵌资源未进模型上下文（v1 不做 MCP 资源工具）", v))
		default:
			out.Opaque = append(out.Opaque, opaquePart("unknown", "未知内容类型，未进模型上下文", c))
		}
	}
	out.Content = strings.Join(texts, "\n")
	return out
}

// opaquePart 造一个非文本内容的占位（原始 JSON 截断）。
func opaquePart(kind, note string, v any) OpaquePart {
	p := OpaquePart{Type: kind, Note: note}
	b, err := json.Marshal(v)
	if err != nil {
		return p
	}
	s := string(b)
	if len(s) > maxOpaqueJSON {
		s = s[:maxOpaqueJSON] + "…(截断)"
	}
	p.JSON = s
	return p
}
