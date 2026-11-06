package sandbox

import (
	"os"
	"strings"
)

// launcherFailPrefix 是「约束后端启动器失败」写进 stderr 的标记。
//
// 父进程据此把这次失败报成**工具错误**（沙箱没装上），而不是「模型命令
// 返回了 125」（正常结果）——两者的处置完全不同：前者要报给宿主
// 「这台机器上路径强制不可用」。
const launcherFailPrefix = "sandbox: 约束启动器失败"

// invocation 是一次**受路径约束执行**的调用描述。
//
// 为什么不是只回一个 argv：路径强制的后端形态不同——macOS 的 Seatbelt 是
// 「把 sandbox-exec 塞到 argv 前面」，Linux 的 Landlock 是「重新执行自己、
// 由启动器在 exec 之前套上约束」。后者要传的东西（环境标记、载荷管道）
// 塞不进 argv，所以这里给一个结构体。
//
// 后端之间形态不同，但**语义必须一致**：拿到同样的 PathScope，
// 约束效果必须一样——否则「同一份宿主配置在两个平台上边界不同」，
// 那种差异没人看得见（fail-closed 的反面：假装有边界）。
type invocation struct {
	// argv 是真正要执行的命令行（已含平台外壳与约束后端的包装）。
	argv []string
	// env 是**追加**到宿主裁剪环境上的变量（约束后端专用，如启动器标记）。
	env []string
	// files 是传给子进程的额外文件描述符（ExtraFiles，约束后端专用）。
	//
	// 调用方负责关闭：cmd.Start 之后子进程才有自己的副本，父进程这份
	// 用完即弃。
	files []*os.File
}

// closeFiles 关闭 invocation 带出来的文件描述符（启动器管道等）。
func (inv invocation) closeFiles() {
	for _, f := range inv.files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// launcherFailure 从输出里找出启动器失败的那一行（没有则返回空串）。
func launcherFailure(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, launcherFailPrefix) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
