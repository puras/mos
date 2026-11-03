package mos_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/puras/mos"
)

// Version 必须是纯三段式（不带 "-dev" 之类后缀）：发版时直接打同名标签，
// 常量与标签一字不差（发布检查清单里就有这一条）。
func TestVersionIsPlainSemver(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(mos.Version) {
		t.Fatalf("Version = %q，应为 x.y.z 形态（与发布标签同名）", mos.Version)
	}
}

// 装配快照要带上框架版本——宿主把它打进日志，才能定位"跑的是哪一版"。
func TestDescribeIncludesVersion(t *testing.T) {
	a, err := mos.New(mos.Config{Model: &stubModel{reply: "ok"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := a.Describe()
	if !strings.Contains(d, "MoS 版本: "+mos.Version) {
		t.Fatalf("Describe 应写出框架版本:\n%s", d)
	}
}
