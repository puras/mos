//go:build linux

package sandbox

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// init 是 Landlock 启动器的入口。
//
// 它在**包初始化阶段**就跑（比 main 早），这样约束一定在 exec 之前生效。
func init() {
	if os.Getenv(landlockLaunchEnv) == "" {
		return
	}
	fdStr := os.Getenv(landlockLaunchFDEnv)
	if fdStr == "" {
		return
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return
	}
	// 载荷管道没开 → 不是启动器。
	//
	// 这是给「宿主环境里恰好有同名环境变量」上的保险：正常启动的服务
	// 进程不该被这段逻辑 exec 掉（那等于自杀）。
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		return
	}
	landlockLaunch(fd)
}

// landlockLaunch 套上约束 → exec 命令。永不返回。
func landlockLaunch(fd int) {
	payload, err := readLandlockFrame(fd)
	if err != nil {
		landlockFail("读载荷失败", err)
	}
	if err := applyLandlock(payload.WriteRoots); err != nil {
		landlockFail("套 Landlock 失败", err)
	}
	if len(payload.Argv) == 0 {
		landlockFail("载荷里没有命令", nil)
	}
	if err := syscall.Exec(payload.Argv[0], payload.Argv, landlockCleanEnv()); err != nil {
		landlockFail("执行命令失败", err)
	}
}

// landlockFail 启动器失败：写 stderr 并以约定的退出码退出。
//
// 退出码 125 是 shell 惯例里的「命令存在但不能执行」；父进程另有标记
// （landlockFailPrefix）区分「沙箱没装上」与「模型命令自己返回 125」。
func landlockFail(msg string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s（Landlock）：%s: %v\n", launcherFailPrefix, msg, err)
	} else {
		fmt.Fprintf(os.Stderr, "%s（Landlock）：%s\n", launcherFailPrefix, msg)
	}
	os.Exit(125)
}

// landlockCleanEnv 去掉启动器标记（被执行的命令不该看到它们）。
func landlockCleanEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, landlockLaunchEnv+"=") || strings.HasPrefix(kv, landlockLaunchFDEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// writeLandlockFrame 写长度前缀的载荷帧。
func writeLandlockFrame(w *os.File, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readLandlockFrame 读长度前缀的载荷帧。
func readLandlockFrame(fd int) (landlockPayload, error) {
	var payload landlockPayload
	hdr := make([]byte, 4)
	if err := readFull(fd, hdr); err != nil {
		return payload, fmt.Errorf("读载荷头失败: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr)
	if n > landlockPayloadMax {
		return payload, fmt.Errorf("载荷过大: %d 字节", n)
	}
	buf := make([]byte, n)
	if err := readFull(fd, buf); err != nil {
		return payload, fmt.Errorf("读载荷失败: %w", err)
	}
	if err := json.Unmarshal(buf, &payload); err != nil {
		return payload, fmt.Errorf("解析载荷失败: %w", err)
	}
	return payload, nil
}

// readFull 读满 buf（unix.Read 可能只读到一部分）。
func readFull(fd int, buf []byte) error {
	for off := 0; off < len(buf); {
		n, err := unix.Read(fd, buf[off:])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if n == 0 {
			return fmt.Errorf("载荷管道提前关闭（读到 %d/%d 字节）", off, len(buf))
		}
		off += n
	}
	return nil
}
