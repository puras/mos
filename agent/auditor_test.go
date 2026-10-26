package agent

import "testing"

func TestDefaultAuditorBuiltinWhitelist(t *testing.T) {
	a := NewDefaultAuditor()
	if !a.Trusted("read_file") || !a.Trusted("current_time") {
		t.Fatalf("内置只读工具应被信任")
	}
	if a.Trusted("shell") || a.Trusted("write_file") {
		t.Fatalf("变更类工具不该被默认信任（需二次取证）")
	}
	// 未知工具：不在白名单 = 不信任（fail-closed）
	if a.Trusted("mystery") {
		t.Fatalf("未知工具不该被信任")
	}
}

func TestDefaultAuditorTrust(t *testing.T) {
	a := NewDefaultAuditor()
	a.Trust("test_runner")
	if !a.Trusted("test_runner") || !a.Trusted("read_file") {
		t.Fatalf("Trust 应在白名单基线上合并，而不是替换")
	}
}

func TestDefaultAuditorCustomList(t *testing.T) {
	// 显式名单以它为准：内置白名单不再生效
	a := &DefaultAuditor{Allow: map[string]bool{"structured_test": true}}
	if !a.Trusted("structured_test") {
		t.Fatalf("自定义名单应生效")
	}
	if a.Trusted("read_file") {
		t.Fatalf("给了自定义名单后内置白名单不应再生效")
	}
	// 空集 = 全不信任（fail-closed，不是"放开"）
	empty := &DefaultAuditor{Allow: map[string]bool{}}
	if empty.Trusted("read_file") {
		t.Fatalf("空名单 = 全不信任")
	}
}

func TestNilAuditorTrustedNothing(t *testing.T) {
	var a *DefaultAuditor
	if a.Trusted("read_file") {
		t.Fatalf("nil 审计器全不信任")
	}
	f := NewDefaultAuditor().Trusted
	if !f("read_file") {
		t.Fatalf("默认审计器应信任只读工具")
	}
}
