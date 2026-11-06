//go:build windows

package sandbox

// shellArgv 把一条命令交给平台外壳解释。
//
// Windows：cmd.exe /C（系统自带，PATH 上一定有）。
//
// 注意：命令语法随之变成 cmd 的（`dir` / `type` / `%VAR%`），不是 POSIX 的
// ——这不是本包的锅：宿主给模型的命令是按目标平台的约定写的。
func shellArgv(command string) []string {
	return []string{"cmd.exe", "/C", command}
}

// resourcePrefix 预置进程资源限制。
//
// Windows 没有 ulimit：CPU/内存上限得靠 Job Object 实施，而 Job Object
// 只能限制**整个 Job** 的总量，做不到「按调用分配预算」。这里不假装有——
// 返回空串（不设限），诚实边界写在包注释里。
func (s *Local) resourcePrefix() string {
	return ""
}
