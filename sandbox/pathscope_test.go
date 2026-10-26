package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

// 默认范围从 WorkDir 派生：读 = 写 = WorkDir（fail-closed 默认：
// 忘了配置 = 只有工作区，不是全盘）。
func TestDeriveScope(t *testing.T) {
	scope, err := deriveScope("/tmp/work")
	if err != nil {
		t.Fatalf("deriveScope: %v", err)
	}
	if len(scope.ReadRoots) != 1 || scope.ReadRoots[0] != "/tmp/work" {
		t.Fatalf("ReadRoots = %v", scope.ReadRoots)
	}
	if len(scope.WriteRoots) != 1 || scope.WriteRoots[0] != "/tmp/work" {
		t.Fatalf("WriteRoots = %v", scope.WriteRoots)
	}
}

// pathWithin：根包含判定（"/" 包容一切）。
func TestPathWithin(t *testing.T) {
	cases := []struct {
		root, p string
		want    bool
	}{
		{"/", "/etc/passwd", true},
		{"/a", "/a", true},
		{"/a", "/a/b", true},
		{"/a", "/ab", false}, // 前缀相同但不是目录边界
		{"/a/b", "/a", false},
	}
	for _, c := range cases {
		if got := pathWithin(c.root, c.p); got != c.want {
			t.Fatalf("pathWithin(%q, %q) = %v, want %v", c.root, c.p, got, c.want)
		}
	}
}

// scopeWithin：请求范围只能是默认范围的子集（只能收窄）。
func TestScopeWithin(t *testing.T) {
	def := &PathScope{ReadRoots: []string{"/w"}, WriteRoots: []string{"/w"}}
	// 子集：合法
	if err := scopeWithin(&PathScope{ReadRoots: []string{"/w"}, WriteRoots: []string{"/w/sub"}}, def); err != nil {
		t.Fatalf("收窄应合法: %v", err)
	}
	// 放宽写：拒绝
	if err := scopeWithin(&PathScope{ReadRoots: []string{"/w"}, WriteRoots: []string{"/w", "/tmp"}}, def); err == nil {
		t.Fatal("放宽写应被拒")
	}
	// 放宽读：拒绝
	if err := scopeWithin(&PathScope{ReadRoots: []string{"/"}, WriteRoots: []string{"/w"}}, def); err == nil {
		t.Fatal("放宽读应被拒")
	}
}

// effectiveScope：nil 请求用默认；放宽的请求被拒。
func TestEffectiveScope(t *testing.T) {
	work := t.TempDir()
	s := &Local{WorkDir: work}

	eff, err := s.effectiveScope(nil)
	if err != nil {
		t.Fatalf("nil 请求应取默认: %v", err)
	}
	if eff.WriteRoots[0] != filepath.Clean(work) {
		t.Fatalf("默认写根 = %v, want %s", eff.WriteRoots, work)
	}

	// 收窄：合法
	if _, err := s.effectiveScope(&PathScope{
		ReadRoots:  []string{work},
		WriteRoots: []string{filepath.Join(work, "sub")},
	}); err != nil {
		t.Fatalf("收窄应合法: %v", err)
	}

	// 放宽：拒绝
	if _, err := s.effectiveScope(&PathScope{ReadRoots: []string{"/"}, WriteRoots: []string{"/"}}); err == nil {
		t.Fatal("放宽应被拒")
	}
	if _, err := s.effectiveScope(&PathScope{
		ReadRoots:  []string{work, "/usr/share"}, // 读根超出默认
		WriteRoots: []string{work},
	}); err == nil {
		t.Fatal("读根放宽应被拒")
	}
}

// 相对路径的根不合法（根必须是绝对路径）。
func TestValidateScopeRejectsRelative(t *testing.T) {
	s := &Local{WorkDir: "/tmp/work"}
	if _, err := s.effectiveScope(&PathScope{ReadRoots: []string{"rel"}, WriteRoots: []string{"rel"}}); err == nil {
		t.Fatal("相对根应被拒")
	}
}

// 写全开的判定：只有显式写 "/" 才算全开。
func TestWriteOpen(t *testing.T) {
	if !writeOpen(&PathScope{WriteRoots: []string{"/"}}) {
		t.Fatal("写根 / 应判为全开")
	}
	if writeOpen(&PathScope{WriteRoots: []string{"/home/a"}}) {
		t.Fatal("普通写根不是全开")
	}
	if writeOpen(nil) {
		t.Fatal("nil 不是全开")
	}
	if !writeOpen(&PathScope{WriteRoots: []string{"/home/a", "/"}}) {
		t.Fatal("包含 / 也算全开")
	}
}

// 全开 + 环境注入：显式 Env 覆盖透传项（宿主注入的变量优先）。
func TestLocalEnvInjection(t *testing.T) {
	t.Setenv("MOS_SANDBOX_PROBE", "from-host")
	s := &Local{WorkDir: t.TempDir(), Env: map[string]string{"MOS_SANDBOX_PROBE": "from-host-config"}}
	// 全开声明：此测试只关心 env，不关心路径强制
	s.DefaultScope = &PathScope{ReadRoots: []string{"/"}, WriteRoots: []string{"/"}}

	res, err := s.Run(t.Context(), Request{Command: "echo $MOS_SANDBOX_PROBE"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "from-host-config") {
		t.Fatalf("显式 Env 应覆盖透传: %s", res.Output)
	}
}
