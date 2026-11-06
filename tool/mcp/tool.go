package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Tool 是接进 MoS 注册表的 MCP 工具。
//
// 它是**普通工具**：走同一套注册表、GuardBus、Policy、审批与
// tool/call / tool/result 流水线——MCP 工具不另开一条道（PI / DSH 共识）。
//
// 三个接口分工：
//   - Name / Description / Parameters：普通工具接口（模型看到的清单）；
//   - Schema：schema 直通（上游给的 JSON Schema 原样透传，不反推）；
//   - Execute：调用上游并把结果投影成 MoS 的返回值。
type Tool struct {
	server string
	raw    string
	name   string
	desc   string
	schema any
	client Client
}

// NewTool 造一个 MCP 工具（server 是服务器名，info 是上游描述）。
func NewTool(server string, info ToolInfo, client Client) *Tool {
	desc := info.Description
	if desc == "" {
		desc = fmt.Sprintf("MCP 工具 %s（服务器 %s）", info.Name, server)
	}
	return &Tool{
		server: server,
		raw:    info.Name,
		name:   ToolName(server, info.Name),
		desc:   desc,
		schema: info.InputSchema,
		client: client,
	}
}

// Name 返回公开工具名（mcp__<server>__<tool>）。
func (t *Tool) Name() string { return t.name }

// Description 返回上游描述。
func (t *Tool) Description() string { return t.desc }

// Parameters 返回 nil——参数 schema 由 Schema() 直通（反射路径对 MCP 无意义）。
func (t *Tool) Parameters() any { return nil }

// Schema 返回上游的 JSON Schema（原样；nil = 降级到宽松 object）。
func (t *Tool) Schema() any { return t.schema }

// Server 返回服务器名。
func (t *Tool) Server() string { return t.server }

// RawName 返回上游原始工具名。
func (t *Tool) RawName() string { return t.raw }

// Execute 调用上游工具并投影结果。
//
// 错误分层（PI / DSH 共识：协议错误与连接错误要分开）：
//   - 上游把错误放进内容并置 isError → 返回 Go error（错误文本带上游内容，
//     模型看得见、可自纠；框架把这次工具结果标成失败）；
//   - 连接断了 / 超时了 / 参数不是 JSON 对象 → 同样是 Go error，但归类不同
//     （传输层 vs 协议层），快照与日志能分清「这台服务器现在不可用」和
//     「这次调用被上游拒绝」。
//
// 成功时返回 *Result（框架会把它 JSON 序列化成工具结果）。
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	var params map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, &Error{
				Kind:   ErrorProtocol,
				Server: t.server,
				Tool:   t.raw,
				Err:    fmt.Errorf("参数不是 JSON 对象: %w", err),
			}
		}
	}
	if t.client == nil {
		return nil, &Error{Kind: ErrorTransport, Server: t.server, Tool: t.raw, Err: errors.New("客户端未连接")}
	}
	res, err := t.client.CallTool(ctx, t.raw, params)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, &Error{Kind: ErrorProtocol, Server: t.server, Tool: t.raw, Err: errors.New("上游没有返回结果")}
	}
	if res.IsError {
		// 上游的错误内容就是给模型的自纠材料——放进 error 的消息里。
		return nil, &Error{Kind: ErrorProtocol, Server: t.server, Tool: t.raw, Err: errors.New(res.Content)}
	}
	return res, nil
}
