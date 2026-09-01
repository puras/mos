// Package tool 提供工具（函数调用）的注册、Schema 生成与执行能力。
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"hegq.net/mos/llm"
)

// Tool 是一个可被 LLM 调用的工具。
// Parameters() 返回参数结构体的零值示例（指针），框架据此生成 JSON Schema；
// Execute 接收模型传来的 JSON 参数，可用 Parse 泛型辅助反序列化。
type Tool interface {
	Name() string
	Description() string
	Parameters() any
	Execute(ctx context.Context, args json.RawMessage) (any, error)
}

// SequentialTool 是可选的执行模式标记（对齐 PI per-tool executionMode）：
// 工具实现该接口且 Sequential() 返回 true 时，该工具在批量执行中被标记为串行项，
// 即它会与同批内其他工具并发执行，但该工具自身的多次调用（如循环重试）会串行进行；
// 或者更常见的情况——整批工具调用中若任意一个实现了 SequentialTool 并返回 true，
// 该项在批内与前后项串行执行，而非与它们并发。
// 注意：这不会让整批退化为串行，只是让该工具在批内串行排队（并发批量路径见 agent/executor）。
type SequentialTool interface {
	Tool
	Sequential() bool
}

// Definition 是工具在协议层的描述。
type Definition struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema
}

// Registry 是线程安全的工具注册表。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// order 保持注册顺序：工具在 prompt 中的描述顺序稳定，利于输出稳定性与 prompt cache。
	order []string
	// schemas 是注册时生成的 JSON Schema 缓存：避免循环内每轮 List() 重复反射。
	schemas map[string]map[string]any
}

// NewRegistry 创建一个空的工具注册表。
func NewRegistry() *Registry {
	return &Registry{
		tools:   make(map[string]Tool),
		schemas: make(map[string]map[string]any),
	}
}

// Register 注册一个工具，重名时返回错误。
func (r *Registry) Register(t Tool) error {
	if t == nil || t.Name() == "" {
		return fmt.Errorf("tool: cannot register nil or unnamed tool")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name()]; dup {
		return fmt.Errorf("tool: duplicate tool %q", t.Name())
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

// Names 返回已注册工具的名称列表。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.order))
	names = append(names, r.order...)
	return names
}

// List 返回工具协议层定义（按注册顺序），供 LLM 请求使用。
// Schema 取自注册时生成的缓存，不在每轮调用中重复反射。
func (r *Registry) List() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		out = append(out, llm.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  r.schemas[name],
		})
	}
	return out
}

// Call 执行指定名称的工具调用。
// 统一入口先做参数容错（normalizeArgs），保证各工具与其包装器拿到干净的单一 JSON 对象。
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	t, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("tool: unknown tool %q", name)
	}
	return t.Execute(ctx, normalizeArgs(args))
}

// Parse 将模型传入的 JSON 参数反序列化为指定结构体类型。
func Parse[T any](args json.RawMessage) (T, error) {
	var out T
	if len(args) == 0 {
		return out, nil
	}
	args = normalizeArgs(args)
	if err := json.Unmarshal(args, &out); err != nil {
		return out, fmt.Errorf("tool: parse args: %w", err)
	}
	return out, nil
}

// normalizeArgs 容错修复模型输出的非法参数 JSON。
// 部分模型偶发输出 "{…}{…}" 这类多个 JSON 值拼接的参数（常为一次想调多个工具），
// 整体解析会失败；此时尝试按顶层对象逐个解码并合并为单一对象，
// 避免整条工具链路中断或仅取第一个对象导致字段丢失。
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

// generateSchema 通过反射从结构体示例生成 JSON Schema。
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

// typeToSchema 将反射类型转换为 JSON Schema 片段。
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
	case reflect.Map:
		return map[string]any{"type": "object"}, nil
	case reflect.Interface:
		return map[string]any{}, nil
	default:
		// 未知类型降级为宽松 schema
		return map[string]any{}, nil
	}
}

// structToSchema 将结构体转换为 object 型 Schema，利用 json / mos tag。
// mos tag 支持 desc=描述 与 required=true|false，例如：mos:"desc=数量,required=true"。
func structToSchema(t reflect.Type) (map[string]any, error) {
	props := map[string]any{}
	var required []string
	for f := range t.Fields() {
		if f.PkgPath != "" { // 未导出字段跳过
			continue
		}
		name, opts := parseJSONTag(f)
		if name == "-" {
			continue
		}
		mosDesc, mosRequired := parseMosTag(f)
		fieldSchema, err := typeToSchema(f.Type)
		if err != nil {
			return nil, err
		}
		if mosDesc != "" {
			fieldSchema["description"] = mosDesc
		}
		props[name] = fieldSchema
		if f.Type.Kind() != reflect.Pointer && !opts["omitempty"] && !mosRequired {
			required = append(required, name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema, nil
}

// parseJSONTag 解析 json tag，返回字段名与选项集合。
func parseJSONTag(f reflect.StructField) (string, map[string]bool) {
	opts := map[string]bool{}
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, opts
	}
	parts := strings.Split(tag, ",")
	for _, o := range parts[1:] {
		opts[o] = true
	}
	if parts[0] == "" {
		return f.Name, opts
	}
	return parts[0], opts
}

// parseMosTag 解析 mos tag，返回描述与是否显式 required。
func parseMosTag(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("mos")
	if tag == "" {
		return "", false
	}
	desc, required := "", false
	for part := range strings.SplitSeq(tag, ",") {
		kv := strings.SplitN(part, "=", 2)
		switch strings.TrimSpace(kv[0]) {
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
