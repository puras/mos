// Package tool 提供工具（函数调用）的接口、注册表与 JSON Schema 生成。
//
// 分层约定：本包只做「工具的登记与调用」，不预设任何具体工具——
// 内置工具集在 tool/builtin，安全约束（Policy / 沙箱 / 文件 jail）在驱动层
// 与 filesystem / sandbox 包，见 docs/ARCHITECTURE.md 包地图。
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/puras/mos/llm"
)

// Tool 是可被 LLM 调用的工具。
// Parameters 返回参数结构体的零值示例（通常是指针），框架据此反射生成 JSON Schema；
// Execute 接收模型传来的 JSON 参数，用 Parse[T] 反序列化。
type Tool interface {
	Name() string
	Description() string
	Parameters() any
	Execute(ctx context.Context, args json.RawMessage) (any, error)
}

// SchemaProvider 是可选接口：工具自带 JSON Schema（跳过反射生成）。
//
// 为什么需要：MCP 工具的 schema 是**上游服务器给的**（tools/list 原样返回），
// MoS 没有对应的结构体可以反射——反推等于瞎猜。实现本接口的工具，注册时
// 直接缓存返回值，Parameters() 的反射结果被忽略。
//
// 契约：返回值必须是合法 JSON Schema 对象（建议 map[string]any 或
// json.RawMessage）；返回 nil = 降级走 Parameters() 反射（fail-open 到老路，
// 不让一个坏 schema 拖垮注册）。
// 守护不变量（FR-C2）：本包产出的 schema 不含 title / $defs——实现者自己
// 保证（MCP 上游 schema 什么形态都可能有，这里不做清洗：原样透传是契约）。
type SchemaProvider interface {
	Schema() any
}

// ConcurrencySafe 是可选接口：工具声明「本次调用可以与同批其他调用并发」。
//
// 契约（对齐 DSH 的 isConcurrencySafe(args)）：
//   - 只有显式返回 true 才 opt-in；未实现本接口、参数非法、返回 false、
//     甚至 panic 一律按**独占**（串行）处理——并发是 fail-closed 的；
//   - 分类器只看**本次调用的参数**，纯同步、无 I/O、无副作用；
//   - 返回 true 是工具的承诺：这次调用与同批其他并发调用不冲突。
//
// 「一元」是有意的取舍：无法表达「这两个写路径不同才安全」这类关系型规则，
// 那种情况一律保持独占（fail-closed）。
//
// 并发只影响**完成顺序**，不影响回填顺序——结果仍按模型的调用序回填。
type ConcurrencySafe interface {
	ConcurrencySafe(args json.RawMessage) bool
}

// Guideline 是可选接口：工具声明「跨调用行为约定」，由装配层注入系统提示词
// （对齐 DSH 的 tool:* 段——跨调用约定属于提示词，不属于某次调用的 schema）。
//
// 分工：**单次调用**怎么传参归 Description（模型构造这条调用时才需要）；
// **跨调用**的约定（结果标记怎么读、先读后写之类的前置条件）归 Guideline——
// 判断标准：模型忘掉它会在下一次调用犯错的，才是 Guideline。
// 文案跟着工具走（工具最懂自己的结果格式），位置由 agent/prompt 统一分配。
// 返回空串 = 本工具不贡献指引。
type Guideline interface {
	Guideline() string
}

// GuidelineText 是工具指引的搬运值（Registry.Guidelines 的元素类型）。
type GuidelineText struct {
	Tool string // 工具名
	Text string // 指引文本
}

// Registry 是线程安全的工具注册表。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// order 保持注册顺序：工具在 prompt 中的描述顺序稳定，利于输出稳定性与 prompt cache。
	order []string
	// schemas 是注册时生成的 JSON Schema 缓存：避免循环内每轮 List() 重复反射。
	schemas map[string]map[string]any
	// deferred 是懒加载集：注册但不进 List()（模型看不到），经 tool_search
	// 激活后下一轮才声明。只记名字，顺序沿 order。
	deferred map[string]bool
	// scope 记每个工具的**代际归属**（Replace 的 scope 参数）：同一 scope 的
	// 上一代工具在下次 Replace 时整体让位（服务器撤掉的工具要消失）；
	// 不同 scope 互不踩——手动 Register 的工具没有 scope（永不被换代波及）。
	scope map[string]string
	// visible 是可见性过滤器层（可选）：所有层都放行的工具才出现在 List() 里，
	// 但仍可被 Call。层按 AND 叠加——policy 名单一层（S5c）、MCP hidden 档
	// 一层（S8.2）。每层带槽名：具名槽可**单独替换**（policy 热替换不会踩掉
	// MCP 那层），空槽名 = 匿名层（SetVisibility / AddVisibility 加的）。
	visible []visibilityLayer
}

// visibilityLayer 是一层可见性过滤器。slot 为空表示匿名层。
type visibilityLayer struct {
	slot string
	fn   func(name string) bool
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{
		tools:    make(map[string]Tool),
		schemas:  make(map[string]map[string]any),
		deferred: make(map[string]bool),
		scope:    make(map[string]string),
	}
}

// Register 注册一个工具；重名或工具无名时返回错误（fail-closed，不静默覆盖）。
func (r *Registry) Register(t Tool) error {
	if t == nil || t.Name() == "" {
		return fmt.Errorf("tool: 不能注册 nil 或无名工具")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name()]; dup {
		return fmt.Errorf("tool: 工具 %q 重复注册", t.Name())
	}
	r.tools[t.Name()] = t
	r.order = append(r.order, t.Name())
	// schema 来源：自带（SchemaProvider）优先，反射生成兜底。
	// 自带返回 nil 同样降级反射（fail-open：一个坏 schema 不该拖垮注册）。
	if sp, ok := t.(SchemaProvider); ok {
		if s := sp.Schema(); s != nil {
			r.schemas[t.Name()] = normalizeProvidedSchema(s)
			return nil
		}
	}
	r.schemas[t.Name()] = generateSchema(t.Parameters())
	return nil
}

// Get 按名称查找工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// normalizeProvidedSchema 把工具自带的 schema 归一成 map[string]any。
//
// 接受 map[string]any（最常见）与 json.RawMessage / []byte（透传字节）两类；
// 其他类型 json.Marshal 兜底；失败返回宽松 object schema（不 panic）。
func normalizeProvidedSchema(s any) map[string]any {
	switch v := s.(type) {
	case map[string]any:
		return v
	case json.RawMessage:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err == nil {
			return m
		}
	case []byte:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err == nil {
			return m
		}
	default:
		if b, err := json.Marshal(s); err == nil {
			var m map[string]any
			if err := json.Unmarshal(b, &m); err == nil {
				return m
			}
		}
	}
	return map[string]any{"type": "object"}
}

// Replace 原子换代：把一组工具（同名覆盖、新名追加）整体替换进注册表，
// schema 缓存与 deferred 标记同步更新。
//
// 为什么是「换代」不是逐个 Register：MCP 工具面会随服务器的 tools/list
// 变化整体换代（S8.2）。逐个换会让清单在中间态被模型看到——半套旧工具
// + 半套新工具。Replace 在锁内一次完成；校验不过时注册表保持原样
// （fail-closed：先校验后落盘，不留半套）。
//
// scope 是**代际归属**：同一 scope 的上一批工具，这次不在名单里的**消失**
// （服务器不再提供它们）；不同 scope 互不踩（服务器 A 换代不会动服务器 B
// 的工具），手动 Register 的工具没有 scope（永不被换代波及）。
//
// 已有的同名工具被覆盖（这正是换代的含义，与 Register 的重名报错相反）。
// deferred[name]=true 的工具以 deferred 态注册；false = 普通注册。
// deferred 标记按新名单**整体重写**（上一代 deferred、这一代 direct 的
// 工具必须真的转成可见——标记残留会让它永远隐身）。
//
// 空名单 = **该代清空**（服务器把工具全撤了）——不是错误。
//
// scope 为空串 = 不参与代际清理（只做注册/覆盖，不动上一代）——给「一次性
// 替换」用的入口；MCP 走的是带 scope 的路径（mcp:<server>）。
func (r *Registry) Replace(scope string, tools []Tool, deferred map[string]bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 先校验整批（名单内重名 + deferred 名单含名单外工具），再落盘。
	seen := make(map[string]bool, len(tools))
	schemas := make(map[string]map[string]any, len(tools))
	for _, t := range tools {
		if t == nil || t.Name() == "" {
			return fmt.Errorf("tool: 不能替换 nil 或无名工具")
		}
		name := t.Name()
		if seen[name] {
			return fmt.Errorf("tool: Replace 名单内工具 %q 重名", name)
		}
		seen[name] = true
		if sp, ok := t.(SchemaProvider); ok {
			if s := sp.Schema(); s != nil {
				schemas[name] = normalizeProvidedSchema(s)
				continue
			}
		}
		schemas[name] = generateSchema(t.Parameters())
	}
	for name := range deferred {
		if !seen[name] {
			return fmt.Errorf("tool: Replace 的 deferred 名单含未在名单中的工具 %q", name)
		}
	}
	// 校验全过。第一步：同一 scope 的上一代里不在新名单的消失
	// （从 tools / schemas / deferred / scope / order 一起清）。
	for _, name := range r.order {
		if r.scope[name] == scope && scope != "" && !seen[name] {
			delete(r.tools, name)
			delete(r.schemas, name)
			delete(r.deferred, name)
			delete(r.scope, name)
		}
	}
	kept := r.order[:0]
	for _, name := range r.order {
		if _, gone := r.tools[name]; gone {
			kept = append(kept, name)
		}
	}
	r.order = kept
	// 第二步：落新名单。已有的只换工具与 schema（order 位置不动）；
	// 新名追加到尾部（顺序稳定，利于 prompt cache）。
	for _, t := range tools {
		name := t.Name()
		if _, exists := r.tools[name]; !exists {
			r.order = append(r.order, name)
		}
		r.tools[name] = t
		r.schemas[name] = schemas[name]
		// deferred 标记按新名单重写（含 false）——上一代 deferred、这一代
		// direct 的工具必须真的转成可见。
		r.deferred[name] = deferred[name]
		if scope != "" {
			r.scope[name] = scope
		}
	}
	return nil
}

// SetVisibility 设置可见性过滤器：返回 false 的工具不出现在 List()（模型看到的
// 清单）里，但仍可被 Call。
//
// 过滤器必须是**无副作用的纯判定**——它会在每次组装工具清单时被调用。
// nil = 清空过滤器。
//
// 本包只认函数值，不认识 policy / hook 之类的具体实现：可见性策略由宿主
// 在装配时接上（例：reg.SetVisibility(policy.New(cfg).Visible)）。
// 注意：本方法是**替换**语义——把可见性栈重置为这一个过滤器。要叠加用
// AddVisibility（policy 与 MCP hidden 各占一层，谁也不覆盖谁）；要只换
// 某一层（如 policy 热替换不动 MCP 层），用 SetVisibilitySlot。
func (r *Registry) SetVisibility(visible func(name string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if visible == nil {
		r.visible = nil
		return
	}
	r.visible = []visibilityLayer{{fn: visible}}
}

// AddVisibility 追加一层可见性过滤器（AND 语义：所有层都放行才可见）。
//
// 为什么是栈不是单值：可见性天然有多个来源——policy 名单一层（S5c）、
// MCP hidden 档一层（S8.2）。单值语义下后设的会覆盖先设的，两层互相
// 踩；栈 + AND 让每层只表达自己那部分判断。
// 过滤器同样必须无副作用；nil 参数忽略。
func (r *Registry) AddVisibility(visible func(name string) bool) {
	if visible == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visible = append(r.visible, visibilityLayer{fn: visible})
}

// SetVisibilitySlot 设置/替换**具名槽**的可见性层：同名槽整层替换，
// 不影响其他槽；fn 为 nil = 移除该槽。
//
// 为什么需要具名槽：policy 的可见性会在运行期热替换（SetPolicy），MCP 的
// hidden 档会在服务器换代时变——两者都是「自己那层变了」，不该动对方的
// 层。匿名层（SetVisibility / AddVisibility）做不到这点：SetVisibility 是
// 整栈替换语义，热替换 policy 会顺手把 MCP 层清掉。
func (r *Registry) SetVisibilitySlot(slot string, visible func(name string) bool) {
	if slot == "" {
		r.SetVisibility(visible)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.visible[:0]
	for _, layer := range r.visible {
		if layer.slot != slot {
			kept = append(kept, layer)
		}
	}
	r.visible = kept
	if visible != nil {
		r.visible = append(r.visible, visibilityLayer{slot: slot, fn: visible})
	}
}

// visibleTo 判断工具在过滤器层下是否可见（调用方须已持锁）。
func (r *Registry) visibleTo(name string) bool {
	for _, layer := range r.visible {
		if layer.fn == nil {
			continue
		}
		if !layer.fn(name) {
			return false
		}
	}
	return true
}

// Names 返回**已注册**的全部工具名（不受可见性过滤影响）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// List 返回工具的协议层定义（按注册顺序），供 LLM 请求使用。
// Schema 取自注册时的缓存，不在每轮调用中重复反射。
// List 返回**模型可见且已激活**的工具清单。
//
// 可见性过滤（SetVisibility）在这里生效：被过滤掉的工具不出现在清单里，模型
// 不会去调它们（PI/DSH 的可见性层语义）。deferred（RegisterDeferred）且未
// 激活的工具同样不出现——它的名字与描述只经 tool_search 暴露（S7.3）。
// Names() 与 Call() 不受两道过滤影响。
func (r *Registry) List() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.Tool, 0, len(r.order))
	for _, name := range r.order {
		if r.deferred[name] {
			continue
		}
		if !r.visibleTo(name) {
			continue
		}
		t := r.tools[name]
		out = append(out, llm.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  r.schemas[name],
		})
	}
	return out
}

// Guidelines 返回已注册工具的跨调用指引（按注册序，稳定；利于 prompt cache）。
// 未实现 Guideline 的工具、返回空串的指引、被可见性过滤掉的工具都不贡献——
// 模型看不见的工具，其指引同样不进提示词（与 List() 同一可见性语义，
// deferred 未激活的同样不进）。
func (r *Registry) Guidelines() []GuidelineText {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []GuidelineText
	for _, name := range r.order {
		if r.deferred[name] {
			continue
		}
		if !r.visibleTo(name) {
			continue
		}
		if gl, ok := r.tools[name].(Guideline); ok {
			if text := gl.Guideline(); text != "" {
				out = append(out, GuidelineText{Tool: name, Text: text})
			}
		}
	}
	return out
}

// RegisterDeferred 注册一个懒加载工具：进注册表（可被 Call、可被守卫裁），
// 但不进 List()/Guidelines()——模型看不到，只有它的名字与描述经
// DeferredTools() 暴露给 tool_search。激活（Activate）后下一轮 List()
// 才声明它（PI 的 deferred exposure / DSH 的 deferLoading 同语义）。
// 重名（含与普通注册的冲突）返回错误。
func (r *Registry) RegisterDeferred(t Tool) error {
	if err := r.Register(t); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferred[t.Name()] = true
	return nil
}

// DeferredInfo 是一个 deferred 工具的搜索元数据（名 + 描述；schema 不进——
// 搜到才加载 schema，这正是省 token 的点）。
type DeferredInfo struct {
	Name        string
	Description string
}

// DeferredTools 返回当前仍未激活的 deferred 工具（按注册序）。已激活的不在——
// 它已进 List()，再列出来是重复信息。
func (r *Registry) DeferredTools() []DeferredInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []DeferredInfo
	for _, name := range r.order {
		if !r.deferred[name] {
			continue
		}
		t := r.tools[name]
		out = append(out, DeferredInfo{Name: name, Description: t.Description()})
	}
	return out
}

// Activate 激活若干 deferred 工具：下一轮 List() 开始声明它们。
// 未注册的名字报错（fail-closed——激活一个不存在的工具是调用方 bug）；
// 未注册为 deferred 的名字（已是普通工具）跳过（它已在清单里）。
// 返回实际新激活的名字（按传入序；已激活的不重复算）。
func (r *Registry) Activate(names []string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var activated []string
	for _, name := range names {
		if _, ok := r.tools[name]; !ok {
			return nil, fmt.Errorf("tool: 激活未注册工具 %q", name)
		}
		if !r.deferred[name] {
			continue // 普通工具已在清单里，跳过
		}
		if r.deferred[name] {
			delete(r.deferred, name)
			activated = append(activated, name)
		}
	}
	return activated, nil
}

// Call 执行指定名称的工具调用（统一入口）。
// 未知工具直接报错（fail-closed）——守卫与策略在驱动层先行拦截，
// 这里的兜底保证未注册的工具绝不会落到任意实现上。
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	t, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("tool: 未知工具 %q", name)
	}
	return t.Execute(ctx, normalizeArgs(args))
}

// Parse 将模型传入的 JSON 参数反序列化为指定类型。
func Parse[T any](args json.RawMessage) (T, error) {
	var out T
	if len(args) == 0 {
		return out, nil
	}
	args = normalizeArgs(args)
	if err := json.Unmarshal(args, &out); err != nil {
		return out, fmt.Errorf("tool: 解析参数: %w", err)
	}
	return out, nil
}

// normalizeArgs 容错修复模型输出的非法参数 JSON。
// 部分模型偶发输出 "{…}{…}" 这类多个 JSON 值拼接的参数（常为一次想调多个工具），
// 整体解析会失败；此时按顶层对象逐个解码并合并为单一对象，
// 避免整条工具链路中断或只取第一个对象导致字段丢失。
func normalizeArgs(args json.RawMessage) json.RawMessage {
	var probe any
	if err := json.Unmarshal(args, &probe); err == nil {
		return args
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	merged := map[string]any{}
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			break
		}
		if obj, ok := v.(map[string]any); ok {
			for k, val := range obj {
				merged[k] = val
			}
		}
	}
	if len(merged) > 0 {
		if b, err := json.Marshal(merged); err == nil {
			return b
		}
	}
	return args
}

// generateSchema 由结构体示例反射生成 JSON Schema。
func generateSchema(example any) map[string]any {
	if example == nil {
		return map[string]any{"type": "object"}
	}
	t := reflect.TypeOf(example)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	schema, _ := typeToSchema(t)
	return schema
}

// typeToSchema 将反射类型转换为 JSON Schema 片段；未知类型降级为宽松 schema。
func typeToSchema(t reflect.Type) (map[string]any, error) {
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Pointer:
		return typeToSchema(t.Elem())
	case reflect.Slice, reflect.Array:
		items, err := typeToSchema(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Struct:
		if t == reflect.TypeOf(struct{}{}) {
			return map[string]any{"type": "object"}, nil
		}
		return structToSchema(t)
	default: // map / interface / 未知
		return map[string]any{}, nil
	}
}

// structToSchema 将结构体转为 object 型 Schema，使用 json 与 mos 两个 tag。
// mos tag 支持 desc=描述 与 required=true|false，例如：
//
//	mos:"desc=数量,required=true"
func structToSchema(t reflect.Type) (map[string]any, error) {
	props := map[string]any{}
	var required []string
	for f := range t.Fields() {
		if f.PkgPath != "" { // 未导出字段跳过
			continue
		}
		name := parseJSONTag(f)
		if name == "-" {
			continue
		}
		desc, must := parseMosTag(f)
		fieldSchema, err := typeToSchema(f.Type)
		if err != nil {
			return nil, err
		}
		if desc != "" {
			fieldSchema["description"] = desc
		}
		props[name] = fieldSchema
		// 必填采用显式声明：仅 mos:"required=true" 的字段进 required 数组。
		// （不按「非指针且无 omitempty 即必填」默认推断——那会让模型被强制补齐
		// 带零值语义的参数；而 required=true 反而不必填，与 tag 语义相悖。）
		if must {
			required = append(required, name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema, nil
}

// parseJSONTag 解析 json tag 取字段名；tag 缺失或 name 项为空时回退 Go 字段名。
// 选项（omitempty 等）不参与必填判定——必填只由 mos tag 显式声明。
func parseJSONTag(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return f.Name
	}
	return name
}

// parseMosTag 解析 mos tag，返回字段描述与是否显式必填。
func parseMosTag(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("mos")
	if tag == "" {
		return "", false
	}
	desc, required := "", false
	for part := range strings.SplitSeq(tag, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		switch kv[0] {
		case "desc":
			if len(kv) == 2 {
				desc = kv[1]
			}
		case "required":
			required = len(kv) == 2 && kv[1] == "true"
		}
	}
	return desc, required
}
