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
}

// NewRegistry 创建一个空的工具注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
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
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	return names
}

// List 返回工具协议层定义，供 LLM 请求使用。
func (r *Registry) List() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, llm.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  generateSchema(t.Parameters()),
		})
	}
	return out
}

// Call 执行指定名称的工具调用。
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	t, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("tool: unknown tool %q", name)
	}
	return t.Execute(ctx, args)
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
// 部分模型偶发输出 "{…}{…}" 这类多个 JSON 值拼接的参数，整体解析会失败；
// 此时尝试取第一个顶层 JSON 值作为参数，避免工具链路整条中断。
func normalizeArgs(args json.RawMessage) json.RawMessage {
	var probe any
	if err := json.Unmarshal(args, &probe); err == nil {
		return args
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	var first any
	if err := dec.Decode(&first); err == nil {
		if b, err := json.Marshal(first); err == nil {
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
	required := []string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
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
	for _, part := range strings.Split(tag, ",") {
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
