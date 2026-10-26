//go:build linux

package builtin

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity 生成 linux 平台的文件身份令牌。
// 字段名与 darwin 不同（Mtim vs Mtimespec）——这是拆平台文件的原因。
func fileIdentity(info os.FileInfo) (string, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d",
		st.Dev, st.Ino, info.Size(), st.Mtim.Nano(), st.Ctim.Nano()), true
}
