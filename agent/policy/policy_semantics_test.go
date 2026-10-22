package policy

import (
	"context"
	"strings"
	"testing"
)

// Allow 未配置 = 不限制（对齐 PI / DSH）。
func TestUnconfiguredAllowDoesNotRestrict(t *testing.T) {
	p := New(Config{Deny: []string{"shell"}})
	if err := p.Check(context.Background(), event("read_file")); err != nil {
		t.Fatalf("Allow 未配置时不应限制: %v", err)
	}
	if err := p.Check(context.Background(), event("shell")); err == nil {
		t.Fatal("Deny 命中的工具应被拒绝")
	}
}

// Allow 为空集 = 全禁（对齐 PI 的 "An empty list exposes no tools"）。
func TestEmptyAllowDeniesEverything(t *testing.T) {
	p := New(Config{Allow: []string{}})
	if err := p.Check(context.Background(), event("read_file")); err == nil {
		t.Fatal("Allow 为空集应全禁")
	}
	if p.Visible("read_file") {
		t.Fatal("Allow 为空集时工具应不可见")
	}
}

// Allow 非空 = 只有名单内可用。
func TestAllowRestrictsToListed(t *testing.T) {
	p := New(Config{Allow: []string{"read_file"}})
	if err := p.Check(context.Background(), event("read_file")); err != nil {
		t.Fatalf("名单内应放行: %v", err)
	}
	if err := p.Check(context.Background(), event("write_file")); err == nil {
		t.Fatal("名单外应拒绝")
	}
}

// Deny 优先于 Allow。
func TestDenyWinsOverAllow(t *testing.T) {
	p := New(Config{Allow: []string{"shell"}, Deny: []string{"shell"}})
	if err := p.Check(context.Background(), event("shell")); err == nil {
		t.Fatal("deny 应优先于 allow")
	}
	if p.Visible("shell") {
		t.Fatal("deny 的工具应不可见")
	}
}

// 通配符：`*` 匹配任意字符（含分隔符），整串锚定（PI 语义）。
func TestWildcardMatching(t *testing.T) {
	p := New(Config{Deny: []string{"write_*", "mcp__*"}})
	for _, name := range []string{"write_file", "write_a_b", "mcp__foo"} {
		if err := p.Check(context.Background(), event(name)); err == nil {
			t.Fatalf("%q 应被通配符拒绝", name)
		}
	}
	// 不匹配的名字放行
	for _, name := range []string{"read_file", "writefile", "my_write_file"} {
		if err := p.Check(context.Background(), event(name)); err != nil {
			t.Fatalf("%q 不该被通配符命中: %v", name, err)
		}
	}
	// 锚定：前缀相同但不在末尾的不该命中
	p2 := New(Config{Deny: []string{"a*b"}})
	if err := p2.Check(context.Background(), event("aXb")); err == nil {
		t.Fatal("a*b 应匹配 aXb（命中即拒）")
	}
	if err := p2.Check(context.Background(), event("aXbc")); err != nil {
		t.Fatalf("a*b 不应匹配 aXbc（整串锚定）: %v", err)
	}
}

// 可见性与运行时判定读同一份名单——不会出现"看不见却能调"的矛盾。
func TestVisibleMatchesCheck(t *testing.T) {
	p := New(Config{Allow: []string{"read_file", "web_*"}, Deny: []string{"read_file"}})
	cases := map[string]bool{
		"read_file":  false, // 在 deny 里
		"web_fetch":  true,  // 匹配 allow 的通配
		"write_file": false, // 既不在 allow 也不在 deny → allow 非空时不许
	}
	for name, want := range cases {
		if got := p.Visible(name); got != want {
			t.Fatalf("Visible(%q) = %v, want %v", name, got, want)
		}
		allowed := p.Check(context.Background(), event(name)) == nil
		if allowed != want {
			t.Fatalf("Check(%q) 放行=%v, 与 Visible 不一致", name, allowed)
		}
	}
}

// nil 策略对象：收紧而非放开（与 Check 的 fail-closed 一致）。
func TestNilPolicyIsRestrictive(t *testing.T) {
	var p *Policy
	if err := p.Check(context.Background(), event("x")); err == nil {
		t.Fatal("nil 策略应拒绝")
	}
	if p.Visible("x") {
		t.Fatal("nil 策略不应让工具可见")
	}
}

// 拒绝理由要写清原因（回填给模型，它据此自我纠正）。
func TestDenialReasonIsActionable(t *testing.T) {
	p := New(Config{Deny: []string{"shell"}})
	err := p.Check(context.Background(), event("shell"))
	if err == nil || !strings.Contains(err.Error(), "shell") {
		t.Fatalf("拒绝理由应点名工具: %v", err)
	}
	if !strings.Contains(err.Error(), "拒绝名单") {
		t.Fatalf("拒绝理由应说明原因: %v", err)
	}
}
