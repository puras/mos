//go:build darwin || freebsd || netbsd || openbsd

package builtin

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity 生成 unix 平台的文件身份令牌：
// dev:ino:size:mtimeNs:ctimeNs（DSH 同款——身份 + 新鲜度元数据）。
// dev/ino 让"删了重建同名文件"也能被识别（新 inode ≠ 旧版本）。
//
// 构建标签只列 darwin/BSD：**不能用 `unix`**——Go 的 unix 标签包含 linux，
// 而 linux 的 Stat_t 字段名不同（Mtim/Ctim，不是 Mtimespec/Ctimespec），
// 加上 linux 另有 observe_version_linux.go → 同一个 fileIdentity 声明两遍，
// 整个包在 Linux 上编译不过。
func fileIdentity(info os.FileInfo) (string, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d",
		st.Dev, st.Ino, info.Size(), st.Mtimespec.Nano(), st.Ctimespec.Nano()), true
}
