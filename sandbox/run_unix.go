//go:build !windows

package sandbox

import (
	"context"
	"time"
)

// run 在 unix（macOS / Linux / BSD）上执行命令。
//
// 路径强制的两种后端形态在这里收敛成同一条流水线：
//
//   - macOS：把 sandbox-exec 塞到 argv 前面（SBPL profile 当场生成）；
//   - Linux：走「重新执行出来的自己」的启动器——Landlock 必须在 exec 之前
//     套到子进程上（Go 没有 fork→exec 的钩子）。
//
// 两者都收敛成 invocation{argv, env, files}：argv 是真正要执行的命令行，
// env 是后端专用标记，files 是后端要的额外 fd。
func (s *Local) run(ctx context.Context, req Request, eff *PathScope, timeout time.Duration) (Result, error) {
	base := shellArgv(s.buildCommand(req.Command))
	inv, err := wrapPathScope(eff, base)
	if err != nil {
		return Result{}, err
	}
	defer inv.closeFiles()

	dir, err := s.checkWorkDir()
	if err != nil {
		return Result{}, err
	}
	return s.runExec(ctx, inv, dir, timeout)
}
