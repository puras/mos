package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MCP 资源工具（S8.4-E，拍板 ②）：**三个共享工具 + 显式 server 参数**。
//
// 参照（docs/ARCHITECTURE.md §3.16 ②，逐项）：
//   - PI：extensions/mcp/resources.ts 的 createMcpResourceToolDefinitions——
//     同样三个工具（list_mcp_resources / list_mcp_resource_templates /
//     read_mcp_resource）；index.ts:467-469 只对「非 hidden 且确实有资源」
//     的服务器注册。
//   - DSH：packages/mcp/mcp-resources/——三个共享工具，**每个调用都要显式
//     server 名**（工具面恒为 3，不随服务器数增长）；分页由 SDK 收集；资源
//     文本进对话历史、二进制只给长度描述（模型读不了 base64）。
//   - 共识：三家高度一致——共享工具、按服务器可见度注册、二进制不进模型。
//     MoS 照抄（工具面恒定），两处自有取舍：
//     工具名统一 `mcp_resource_*` 前缀（两家都不带 mcp__ 前缀、与工具命名
//     不统一——§3.16 空白项的拍板）；二进制走 MoS 已有的 OpaquePart 占位
//     （与 CallTool 的图像/资源降级同一条路），不是自创机制。
//
// 注册口径：只对「连接成功、非 hidden 档（Exposure != hidden）、且连接后
// 确实声明了资源或模板」的服务器可达——hidden 服务器的资源不泄露（模型连
// 服务器名都拿不到，资源更不能给）；没有资源的服务器不注册（避免模型拿到
// 一组永远 404 的工具，PI 的 hasResources 同款口径）。

// 三个共享工具的名字（mcp_resource_* 前缀：与 mcp__<server>__<tool> 的
// 单服务器工具区分开，避免模型把服务器名混进工具名）。
const (
	ResourceListName  = "mcp_resource_list"
	ResourceTemplName = "mcp_resource_templates"
	ResourceReadName  = "mcp_resource_read"
)

// ResourceListTool 列出某台 MCP 服务器暴露的资源。
type ResourceListTool struct {
	mgr *Manager
}

// Name 实现.tool.Tool。
func (t *ResourceListTool) Name() string { return ResourceListName }

// Description 实现tool.Tool。
func (t *ResourceListTool) Description() string {
	return "列出某台 MCP 服务器暴露的资源（server 用系统提示词 mcp_servers 节里的服务器名）。"
}

// Parameters 实现tool.Tool（框架反射生成 JSON Schema）。
type resourceListParams struct {
	Server string `json:"server" mos:"desc=服务器名（见 mcp_servers 节）,required=true"`
}

func (t *ResourceListTool) Parameters() any { return &resourceListParams{} }

// Execute 实现tool.Tool。
func (t *ResourceListTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	var p resourceListParams
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("参数不合法: %w", err)
		}
	}
	s := t.mgr.visibleServer(p.Server)
	if s == nil {
		return nil, fmt.Errorf("未知服务器 %q（或该服务器未暴露资源）；可用服务器见系统提示词 mcp_servers 节", p.Server)
	}
	infos, err := s.client.ListResources(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"server": p.Server, "resources": resourcesJSON(infos)}, nil
}

// ResourceTemplatesTool 列出某台 MCP 服务器的资源模板（URI 模板，可带参数构造资源）。
type ResourceTemplatesTool struct {
	mgr *Manager
}

// Name 实现tool.Tool。
func (t *ResourceTemplatesTool) Name() string { return ResourceTemplName }

// Description 实现tool.Tool。
func (t *ResourceTemplatesTool) Description() string {
	return "列出某台 MCP 服务器的资源模板（URI 模板；按模板填参数得到资源 URI，再用 mcp_resource_read 读取）。"
}

// Parameters 实现tool.Tool。
type resourceTemplatesParams struct {
	Server string `json:"server" mos:"desc=服务器名（见 mcp_servers 节）,required=true"`
}

func (t *ResourceTemplatesTool) Parameters() any { return &resourceTemplatesParams{} }

// Execute 实现tool.Tool。
func (t *ResourceTemplatesTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	var p resourceTemplatesParams
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("参数不合法: %w", err)
		}
	}
	s := t.mgr.visibleServer(p.Server)
	if s == nil {
		return nil, fmt.Errorf("未知服务器 %q（或该服务器未暴露资源）；可用服务器见系统提示词 mcp_servers 节", p.Server)
	}
	infos, err := s.client.ListResourceTemplates(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"server": p.Server, "templates": resourceTemplatesJSON(infos)}, nil
}

// ResourceReadTool 读取某台 MCP 服务器上的一个资源。
//
// 文本内容直接进工具结果（模型可读）；二进制内容降级成 OpaquePart 占位
// （模型读不了 base64；程序化调用方需要完整内容时自己走 MCP 协议调——
// 与 CallTool 的图像/资源降级同一条路）。
type ResourceReadTool struct {
	mgr *Manager
}

// Name 实现tool.Tool。
func (t *ResourceReadTool) Name() string { return ResourceReadName }

// Description 实现tool.Tool。
func (t *ResourceReadTool) Description() string {
	return "读取某台 MCP 服务器上的一个资源（uri 用 mcp_resource_list 列出的资源 URI；文本内容直接返回，二进制只给占位说明）。"
}

// Parameters 实现tool.Tool。
type resourceReadParams struct {
	Server string `json:"server" mos:"desc=服务器名（见 mcp_servers 节）,required=true"`
	URI    string `json:"uri" mos:"desc=资源 URI（mcp_resource_list 或 mcp_resource_templates 得到）,required=true"`
}

func (t *ResourceReadTool) Parameters() any { return &resourceReadParams{} }

// Execute 实现tool.Tool。
func (t *ResourceReadTool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	var p resourceReadParams
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("参数不合法: %w", err)
		}
	}
	s := t.mgr.visibleServer(p.Server)
	if s == nil {
		return nil, fmt.Errorf("未知服务器 %q（或该服务器未暴露资源）；可用服务器见系统提示词 mcp_servers 节", p.Server)
	}
	contents, err := s.client.ReadResource(ctx, p.URI)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"server": p.Server, "uri": p.URI}
	var texts []string
	var opaque []OpaquePart
	for _, rc := range contents {
		if len(rc.Blob) > 0 {
			opaque = append(opaque, opaquePart("resource", fmt.Sprintf(
				"二进制资源未进模型上下文（%d 字节，%s）：需要完整内容请走 MCP 协议读取",
				len(rc.Blob), mimeOr(rc.MIMEType, "未知类型")), rc))
			continue
		}
		texts = append(texts, rc.Text)
	}
	if len(texts) > 0 {
		out["content"] = strings.Join(texts, "\n")
	}
	if len(opaque) > 0 {
		b, err := json.Marshal(opaque)
		if err == nil {
			out["opaque"] = json.RawMessage(b)
		}
	}
	return out, nil
}

// registerResourceTools 把资源工具注册进注册表（装配在 Start 之后调）。
//
// 只要**任一台**可见服务器有资源就注册全部三个工具——工具面是共享的，
// per-server 会导致清单随服务器数抖动。服务器间路由靠显式 server 参数。
//
// scope 为空串（不参与代际清理）：资源工具不随单台服务器的工具换代消失
// （换代清的是 mcp:<server> scope；「服务器不再有资源」由下次装配决定）。
func (m *Manager) registerResourceTools() error {
	list := &ResourceListTool{mgr: m}
	templ := &ResourceTemplatesTool{mgr: m}
	read := &ResourceReadTool{mgr: m}
	if err := m.reg.Register(list); err != nil {
		return err
	}
	if err := m.reg.Register(templ); err != nil {
		return err
	}
	return m.reg.Register(read)
}

// visibleServer 按名字找一台「资源可达」的服务器：已连接、非 hidden 档、
// 连接后确实声明过资源或模板。找不到返回 nil（三个工具共用同一判定——
// hidden 服务器的资源不泄露）。
func (m *Manager) visibleServer(name string) *Server {
	s := m.serverByName(name)
	if s == nil || m.serverHidden(s.cfg) || !s.hasResources() {
		return nil
	}
	return s
}

// serverHidden 判断一台服务器整体上对模型隐藏（服务器级 hidden，或逐工具
// 通配 hidden —— 后者意味着模型看不见这台服务器的任何工具）。
func (m *Manager) serverHidden(cfg ServerConfig) bool {
	if cfg.Exposure == ExposureHidden {
		return true
	}
	if e, ok := cfg.ToolExposure["*"]; ok {
		return ParseExposure(string(e)) == ExposureHidden
	}
	return false
}

// resourcesJSON 把资源列表投成给模型看的 JSON（确定性顺序：按 URI 排序）。
func resourcesJSON(infos []ResourceInfo) []map[string]any {
	sorted := append([]ResourceInfo(nil), infos...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].URI < sorted[j].URI })
	out := make([]map[string]any, 0, len(sorted))
	for _, r := range sorted {
		m := map[string]any{"uri": r.URI}
		if r.Name != "" {
			m["name"] = r.Name
		}
		if r.Description != "" {
			m["description"] = r.Description
		}
		if r.MIMEType != "" {
			m["mimeType"] = r.MIMEType
		}
		if r.Size > 0 {
			m["size"] = r.Size
		}
		out = append(out, m)
	}
	return out
}

// resourceTemplatesJSON 同上（按 uriTemplate 排序）。
func resourceTemplatesJSON(infos []ResourceTemplateInfo) []map[string]any {
	sorted := append([]ResourceTemplateInfo(nil), infos...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].URITemplate < sorted[j].URITemplate })
	out := make([]map[string]any, 0, len(sorted))
	for _, r := range sorted {
		m := map[string]any{"uriTemplate": r.URITemplate}
		if r.Name != "" {
			m["name"] = r.Name
		}
		if r.Description != "" {
			m["description"] = r.Description
		}
		if r.MIMEType != "" {
			m["mimeType"] = r.MIMEType
		}
		out = append(out, m)
	}
	return out
}

// mimeOr 是 mime 的空串兜底。
func mimeOr(mime, fallback string) string {
	if mime == "" {
		return fallback
	}
	return mime
}
