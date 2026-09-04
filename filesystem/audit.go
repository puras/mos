package filesystem

import (
	"context"

	"github.com/puras/mos/agent/event"
)

// Bus 是事件发射抽象（*event.EventBus 的结构化子集）。
// fs 只需要 emit 一种模式，故不依赖完整 EventBus 类型，便于测试替身与平台自定义实现。
type Bus interface {
	Emit(ctx context.Context, name string, payload any) error
}

type busKey struct{}

// WithBus 把事件总线注入 ctx（与 WithFileSystem 同形）。
// Sandboxed.Resolve 据此发 fs/resolve 审计事件；未注入时静默跳过（审计可选）。
func WithBus(ctx context.Context, bus Bus) context.Context {
	return context.WithValue(ctx, busKey{}, bus)
}

// BusFromContext 从 ctx 取出事件总线；未注入返回 nil。
func BusFromContext(ctx context.Context) Bus {
	v, _ := ctx.Value(busKey{}).(Bus)
	return v
}

// emitResolve 发 fs/resolve 审计事件。bus 未注入时静默返回。
// reason 仅在 denied 时有意义（allowed 传空串）。
func emitResolve(ctx context.Context, req Request, mode Mode, target, workspace, decision, reason string) {
	bus := BusFromContext(ctx)
	if bus == nil {
		return
	}
	_ = bus.Emit(ctx, event.SessionEventFsResolve, event.FsResolveData{
		Display:   req.Path,
		Target:    target,
		Op:        string(req.Op),
		Mode:      string(mode),
		Decision:  decision,
		Workspace: workspace,
		Reason:    reason,
		SessionID: req.SessionID,
	})
}
