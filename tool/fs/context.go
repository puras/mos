package fs

import "context"

type ctxKey struct{}

// WithFileSystem 把 FileSystem 注入 ctx。
func WithFileSystem(ctx context.Context, fsys FileSystem) context.Context {
	return context.WithValue(ctx, ctxKey{}, fsys)
}

// FromContext 从 ctx 取出 FileSystem；未注入返回 nil。
// 工具应在调用方转为 *NotConfiguredError（fail-closed）。
func FromContext(ctx context.Context) FileSystem {
	v, _ := ctx.Value(ctxKey{}).(FileSystem)
	return v
}
