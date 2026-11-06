//go:build windows

package sandbox

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// windowsLaunchEnv 是启动器标记：子进程靠它认出「我是被沙箱拉起来的那个」。
	windowsLaunchEnv = "MOS_SANDBOX_WIN_LAUNCH"
	// windowsPIDFileEnv 是 pid 文件路径（启动器把自己的 PID 写进去，
	// 父进程超时时要靠它精确杀整棵树——库不暴露子进程句柄）。
	windowsPIDFileEnv = "MOS_SANDBOX_WIN_PID_FILE"
	// windowsPayloadMax 是载荷上限（管道缓冲区 64KB，留足余量）。
	windowsPayloadMax = 32 << 10
)

// windowsPayload 是父进程交给启动器的载荷。
type windowsPayload struct {
	// Argv 是要执行的命令行（cmd.exe /C ...）。
	Argv []string `json:"argv"`
}

// init 是 Windows 启动器的入口。
//
// 它在**包初始化阶段**就跑（比 main 早），这样 token 一定在命令开始前生效。
func init() {
	if os.Getenv(windowsLaunchEnv) == "" {
		return
	}
	windowsLaunch()
}

// windowsLaunch 报 PID → 跑命令 → 用命令的退出码退出（永不返回）。
func windowsLaunch() {
	// 先报 PID：父进程超时时要靠它精确杀整棵树。
	if path := os.Getenv(windowsPIDFileEnv); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	payload, err := readWindowsFrame(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s（Windows）：读载荷失败: %v\n", launcherFailPrefix, err)
		os.Exit(125)
	}
	if len(payload.Argv) == 0 {
		fmt.Fprintf(os.Stderr, "%s（Windows）：载荷里没有命令\n", launcherFailPrefix)
		os.Exit(125)
	}
	cmd := exec.Command(payload.Argv[0], payload.Argv[1:]...)
	// stdin 是载荷管道（已读到 EOF）→ 命令看到空输入（等价 /dev/null）。
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = windowsCleanEnv()
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "%s（Windows）：执行命令失败: %v\n", launcherFailPrefix, err)
		os.Exit(125)
	}
	os.Exit(0)
}

// windowsCleanEnv 去掉启动器标记（被执行的命令不该看到它们）。
func windowsCleanEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, windowsLaunchEnv+"=") || strings.HasPrefix(kv, windowsPIDFileEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// writeWindowsFrame 写长度前缀的载荷帧。
func writeWindowsFrame(w *os.File, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readWindowsFrame 读长度前缀的载荷帧。
func readWindowsFrame(f *os.File) (windowsPayload, error) {
	var payload windowsPayload
	hdr := make([]byte, 4)
	if _, err := f.Read(hdr); err != nil {
		return payload, fmt.Errorf("读载荷头失败: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr)
	if n > windowsPayloadMax {
		return payload, fmt.Errorf("载荷过大: %d 字节", n)
	}
	buf := make([]byte, n)
	read := 0
	for read < len(buf) {
		m, err := f.Read(buf[read:])
		if err != nil {
			return payload, fmt.Errorf("读载荷失败: %w", err)
		}
		if m == 0 {
			return payload, fmt.Errorf("载荷管道提前关闭（读到 %d/%d 字节）", read, len(buf))
		}
		read += m
	}
	if err := json.Unmarshal(buf, &payload); err != nil {
		return payload, fmt.Errorf("解析载荷失败: %w", err)
	}
	return payload, nil
}
