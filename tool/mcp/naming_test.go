package mcp

import (
	"strings"
	"testing"
)

func TestToolName(t *testing.T) {
	cases := []struct {
		server, tool, want string
	}{
		{"github", "create_issue", "mcp__github__create_issue"},
		{"my-server", "do.thing", "mcp__my_server__do_thing"},
		{"s", "t", "mcp__s__t"},
	}
	for _, c := range cases {
		if got := ToolName(c.server, c.tool); got != c.want {
			t.Fatalf("ToolName(%q,%q) = %q，期望 %q", c.server, c.tool, got, c.want)
		}
	}
	// 非 ASCII 名字：逐字节换成 _（名字是标识符，不是文本）。
	got := ToolName("带中文", "工具")
	if !strings.HasPrefix(got, "mcp__") {
		t.Fatalf("应保留命名空间前缀：%q", got)
	}
	for i := 0; i < len(got); i++ {
		if got[i] > 127 {
			t.Fatalf("名字里不该有非 ASCII 字节：%q", got)
		}
	}
}

func TestToolNameTooLong(t *testing.T) {
	long := strings.Repeat("x", 80)
	name := ToolName("server", long)
	if len(name) > MaxToolNameLength {
		t.Fatalf("超长名没收短：%d 字节", len(name))
	}
	if !strings.HasSuffix(name, "_"+name[len(name)-hashSuffixLength:]) {
		t.Fatalf("应带哈希后缀：%q", name)
	}
	// 同名稳定（两次算出来一样——prompt cache 不能抖）。
	if again := ToolName("server", long); again != name {
		t.Fatalf("同输入应得同输出：%q vs %q", name, again)
	}
	// 不同的长名 → 不同的后缀（不能撞成同一个名字）。
	other := ToolName("server", long+"y")
	if other == name {
		t.Fatalf("不同的超长名不该撞名：%q", other)
	}
}

func TestParseExposure(t *testing.T) {
	if got := ParseExposure("direct"); got != ExposureDirect {
		t.Fatalf("direct 应解析成 direct：%q", got)
	}
	if got := ParseExposure("HIDDEN"); got != ExposureHidden {
		t.Fatalf("hidden 应解析成 hidden：%q", got)
	}
	// 未知值退到最保守的一档（模型看不见）。
	if got := ParseExposure("codemode"); got != ExposureDeferred {
		t.Fatalf("未知值应退到 deferred：%q", got)
	}
	if got := ParseExposure(""); got != ExposureDeferred {
		t.Fatalf("空值应退到 deferred：%q", got)
	}
}
