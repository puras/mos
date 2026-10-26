//go:build unix

package builtin

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity 生成 unix 平台的文件身份令牌：
// dev:ino:size:mtimeNs:ctimeNs（DSH 同款——身份 + 新鲜度元数据）。
// dev/ino 让"删了重建同名文件"也能被识别（新 inode ≠ 旧版本）。
func fileIdentity(info os.FileInfo) (string, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d",
		st.Dev, st.Ino, info.Size(), st.Mtimespec.Nano(), st.Ctimespec.Nano()), true
}
