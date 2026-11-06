//go:build !darwin && !linux && !windows

package sandbox

import "fmt"

// 其余平台（BSD 等）：路径强制后端尚未落地。
//
// fail-closed：范围非全开时明确报错，**绝不退化成"没隔离的假装隔离"**。
// 想在当前平台用 shell，宿主显式声明写根含 "/"（容器场景的正当用法）——
// 危险写在脸上，横幅与日志都看得见。
func wrapPathScope(eff *PathScope, base []string) (invocation, error) {
	if writeOpen(eff) {
		return invocation{argv: base}, nil
	}
	return invocation{}, fmt.Errorf("%w: 此平台尚无路径强制后端（darwin 用 Seatbelt、linux 用 Landlock）；确需放开请在宿主显式声明写根 \"/\"", ErrPathScopeUnsupported)
}
