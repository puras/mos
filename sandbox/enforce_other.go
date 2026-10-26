//go:build !darwin

package sandbox

import (
	"fmt"
)

// 非 darwin 平台：路径强制后端尚未落地（Linux 走 Landlock，待实现）。
//
// fail-closed：范围非全开时明确报错，**绝不退化成"没隔离的假装隔离"**。
// 想在当前平台用 shell，宿主显式声明写根含 "/"（容器场景的正当用法）——
// 危险写在脸上，横幅与日志都看得见。

// wrapPathScope 把命令 argv 包进路径约束（此平台：仅全开放行）。
func wrapPathScope(eff *PathScope, base []string) ([]string, error) {
	if writeOpen(eff) {
		return base, nil
	}
	return nil, fmt.Errorf("%w: 此平台尚无路径强制后端（Linux Landlock 待实现）；确需放开请在宿主显式声明写根 \"/\"", ErrPathScopeUnsupported)
}
