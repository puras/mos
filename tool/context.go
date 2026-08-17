package tool

import "context"

// 工具执行上下文辅助：driver 在调用工具前经 ctx 注入调用上下文（当前调用 ID 等），
// 供工具实现（如 request_human_input）读取。与 WithSandbox 同一模式。

type callIDCtxKey struct{}

// WithCallID 将当前工具调用 ID 放入上下文，供工具执行时取用。
func WithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDCtxKey{}, id)
}

// CallIDFrom 从上下文读取当前工具调用 ID（未注入时为空字符串）。
func CallIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(callIDCtxKey{}).(string)
	return s
}
