//go:build darwin

package sandbox

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// macOS 的路径强制：Seatbelt（sandbox_exec + SBPL profile）。
//
// profile 形态对齐 DSH（packages/sandbox/sandbox-local/src/profiles.ts，实战
// 调过的最小集）：**只收窄写**——
//
//	(version 1)
//	(allow default)                                   ← 其余全放（读、进程、网络）
//	(deny file-write*)                                ← 否决所有写
//	(allow file-write* (literal "/dev/null"))         ← /dev/null 例外
//	(allow file-write* (subpath "<写根>") ...)         ← 放行写根
//
// 为什么不连读一起收：deny file-read* 会让动态链接器、系统缓存、字体服务
// 全部碎掉，调 profile 的成本远超收益；而且事故威胁模型（误删、越界写）
// 都在写这一侧。**ReadRoots 在此后端暂不强制**（校验仍做，强制待 Landlock）。
//
// 内核级强化的好处：子进程继承约束、符号链接在 vnode 层解析（软链逃逸无效）、
// 与命令怎么拼写无关——`git rm -r` 和逐个 `git rm` 删的都是同一个禁区。

// seatbeltBin 是 macOS 自带的沙箱执行器（Apple 未公开文档化，但系统自带）。
const seatbeltBin = "/usr/bin/sandbox-exec"

// wrapPathScope 把命令 argv 包进路径约束。
//
// 写全开（写根含 "/"）→ 原样返回：没有要强制的边界，也不做样子。
// 其他情况：sandbox-exec 可用则包 profile；不可用 → ErrPathScopeUnsupported
// （fail-closed：绝不退化成"没隔离的假装隔离"）。
func wrapPathScope(eff *PathScope, base []string) ([]string, error) {
	if writeOpen(eff) {
		return base, nil
	}
	if _, err := exec.LookPath(seatbeltBin); err != nil {
		return nil, fmt.Errorf("%w: sandbox-exec 不可用（%s 缺失？）", ErrPathScopeUnsupported, seatbeltBin)
	}
	profile, err := seatbeltProfile(eff.WriteRoots)
	if err != nil {
		return nil, err
	}
	argv := make([]string, 0, len(base)+3)
	argv = append(argv, seatbeltBin, "-p", profile)
	argv = append(argv, base...)
	return argv, nil
}

// seatbeltProfile 生成 SBPL profile（一行形态，DSH 同款）。
func seatbeltProfile(writeRoots []string) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1) (allow default) (deny file-write*)")
	b.WriteString(` (allow file-write* (literal "/dev/null"))`)
	for _, root := range writeRoots {
		// SBPL 按内核解析后的路径匹配：软链根（如 /var → /private/var）必须
		// 先规范化，否则写根匹配不上——写工作区会被自己拦住。
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			// 根尚不存在（宿主声明了"以后会创建"的目录）：退回清洁后的
			// 绝对路径。写一个不存在的根本来就会失败，不会静默放宽。
			canonical = filepath.Clean(root)
		}
		b.WriteString(" (allow file-write* (subpath " + sbplString(canonical) + "))")
	}
	return b.String(), nil
}

// sbplString 把路径引成 SBPL 字符串字面量（转义反斜杠与双引号）。
func sbplString(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(p) + `"`
}
