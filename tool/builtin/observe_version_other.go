//go:build !unix

package builtin

import (
	"fmt"
	"os"
)

// fileIdentity：非 unix 平台拿不到 dev/ino/ctime（Go 的 FileInfo.Sys() 是
// 平台私有结构），退化为 size:mtimeNs——弱一些（同尺寸同刻的写入不可区分），
// 文档已注明。
func fileIdentity(info os.FileInfo) (string, bool) {
	return "", false
}

// 保留 fmt 引用：上面留空实现时 fallback 用 size:mtime（在 observe.go）。
var _ = fmt.Sprintf
