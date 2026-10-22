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

// Registry 是线程安全的工具注册表。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// order 保持注册顺序：工具在 prompt 中的描述顺序稳定，利于输出稳定性与 prompt cache。
	order []string
	// schemas 是注册时生成的 JSON Schema 缓存：避免循环内每轮 List() 重复反射。
	schemas map[string]map[string]any
	// visible 是可见性过滤器（可选）：返回 false 的工具不出现在 List() 里，
	// 但仍可被 Call——执行与否由运行时守卫决定。
	visible func(name string) bool
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{
		tools:   make(map[string]Tool),
		schemas: make(map[string]map[string]any),
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

// Names 返回已注册工具名（按注册顺序）。
// SetVisibility 设置可见性过滤器：返回 false 的工具不出现在 List()（模型看到的
// 清单）里，但仍可被 Call。
//
// 过滤器必须是**无副作用的纯判定**——它会在每次组装工具清单时被调用。
// nil = 不过滤（全部可见）。
//
// 本包只认一个函数值，不认识 policy / hook 之类的具体实现：可见性策略由宿主
// 在装配时接上（例：reg.SetVisibility(policy.New(cfg).Visible)）。
func (r *Registry) SetVisibility(visible func(name string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visible = visible
}

// Names 返回**已注册**的全部工具名（不受可见性过滤影响）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// List 返回工具的协议层定义（按注册顺序），供 LLM 请求使用。
// Schema 取自注册时的缓存，不在每轮调用中重复反射。
// List 返回**模型可见**的工具清单。
//
// 可见性过滤（SetVisibility）在这里生效：被过滤掉的工具不出现在清单里，模型
// 不会去调它们（PI/DSH 的可见性层语义）。Names() 与 Call() 不受过滤影响。
func (r *Registry) List() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.Tool, 0, len(r.order))
	for _, name := range r.order {
		if r.visible != nil && !r.visible(name) {
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
