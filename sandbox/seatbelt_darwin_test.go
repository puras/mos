//go:build darwin

package sandbox

import (
	"strings"
	"testing"
)

// profile 形态：默认全放 → 否决写 → /dev/null 例外 → 放行写根（DSH 同款）。
func TestSeatbeltProfile(t *testing.T) {
	p, err := seatbeltProfile([]string{"/tmp/work"})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}
	for _, want := range []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (literal "/dev/null"))`,
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("profile 缺 %q: %s", want, p)
		}
	}
	if !strings.Contains(p, `(subpath "/private/tmp/work")`) && !strings.Contains(p, `(subpath "/tmp/work")`) {
		// 根会被规范化（/tmp 是 /private/tmp 的软链）：两者之一必须出现
		t.Fatalf("写根应出现在 profile 里: %s", p)
	}
}

// SBPL 字面量转义：路径里的反斜杠与双引号不能破坏 profile 结构。
func TestSeatbeltProfileEscaping(t *testing.T) {
	p, err := seatbeltProfile([]string{`/tmp/we"ird\path`})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}
	// 引号必须被转义，否则 profile 会被路径里的引号截断
	if !strings.Contains(p, `we\"ird\\path`) {
		t.Fatalf("路径未转义: %s", p)
	}
}

// 多个写根：每个都进 profile。
func TestSeatbeltProfileMultipleRoots(t *testing.T) {
	p, err := seatbeltProfile([]string{"/tmp/a", "/tmp/b"})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}
	if strings.Count(p, "(allow file-write* (subpath") != 2 {
		t.Fatalf("两个写根都应放行: %s", p)
	}
}
