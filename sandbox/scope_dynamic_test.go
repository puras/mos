package sandbox

import (
	"path/filepath"
	"sync"
	"testing"
)

// 动态范围：宿主在运行期放宽自己的默认范围（用户在会话中授权新目录）。
//
// 语义分工（别混）：
//   - 宿主改**默认范围**（Scope）= 宿主的政策，运行期可做；
//   - 调用方（模型参数）给 Request.PathScope = 只能**收窄**，放宽一律拒绝。
func TestScopeFuncDynamicWidening(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()

	var mu sync.RWMutex
	roots := []string{a}
	scopeFunc := ScopeFunc(
		func() []string { mu.RLock(); defer mu.RUnlock(); return append([]string(nil), roots...) },
		func() []string { mu.RLock(); defer mu.RUnlock(); return append([]string(nil), roots...) },
	)
	s := &Local{WorkDir: a, Scope: scopeFunc}

	// 初始：只有 A。
	eff, err := s.effectiveScope(nil)
	if err != nil {
		t.Fatalf("effectiveScope: %v", err)
	}
	if len(eff.WriteRoots) != 1 || eff.WriteRoots[0] != filepath.Clean(a) {
		t.Fatalf("初始写根 = %v，want [%s]", eff.WriteRoots, a)
	}

	// 运行期加根：宿主授权了 B。
	mu.Lock()
	roots = append(roots, b)
	mu.Unlock()

	eff, err = s.effectiveScope(nil)
	if err != nil {
		t.Fatalf("effectiveScope: %v", err)
	}
	if len(eff.WriteRoots) != 2 {
		t.Fatalf("加根后写根 = %v，want 2 个", eff.WriteRoots)
	}

	// 调用方仍不能超出当前默认范围（放宽不是模型的权力）。
	if _, err := s.effectiveScope(&PathScope{ReadRoots: []string{"/"}, WriteRoots: []string{"/"}}); err == nil {
		t.Fatal("调用方放宽范围应被拒")
	}
	// 收窄仍然合法。
	if _, err := s.effectiveScope(&PathScope{
		ReadRoots:  []string{a},
		WriteRoots: []string{filepath.Join(a, "sub")},
	}); err != nil {
		t.Fatalf("收窄应合法: %v", err)
	}
}

// Scope 返回 nil = 没给出范围，回落到 DefaultScope / WorkDir 派生（不是"放开"）。
func TestScopeFuncNilFallsBack(t *testing.T) {
	work := t.TempDir()
	s := &Local{WorkDir: work, Scope: func() *PathScope { return nil }}
	eff, err := s.effectiveScope(nil)
	if err != nil {
		t.Fatalf("effectiveScope: %v", err)
	}
	if len(eff.WriteRoots) != 1 || eff.WriteRoots[0] != filepath.Clean(work) {
		t.Fatalf("回落的写根 = %v，want [%s]", eff.WriteRoots, work)
	}

	// 显式 DefaultScope 优先于派生。
	s2 := &Local{WorkDir: work, DefaultScope: &PathScope{ReadRoots: []string{"/w"}, WriteRoots: []string{"/w"}}}
	eff2, err := s2.effectiveScope(nil)
	if err != nil {
		t.Fatalf("effectiveScope: %v", err)
	}
	if eff2.WriteRoots[0] != "/w" {
		t.Fatalf("默认范围 = %v，want [/w]", eff2.WriteRoots)
	}
}

// Scope 优先于 DefaultScope（动态来源是"活的"那份）。
func TestScopeFuncBeatsDefaultScope(t *testing.T) {
	s := &Local{
		WorkDir:      t.TempDir(),
		DefaultScope: &PathScope{ReadRoots: []string{"/w"}, WriteRoots: []string{"/w"}},
		Scope:        func() *PathScope { return &PathScope{ReadRoots: []string{"/d"}, WriteRoots: []string{"/d"}} },
	}
	eff, err := s.effectiveScope(nil)
	if err != nil {
		t.Fatalf("effectiveScope: %v", err)
	}
	if eff.WriteRoots[0] != "/d" {
		t.Fatalf("应取 Scope 的范围，得到 %v", eff.WriteRoots)
	}
}

// 读写分开：ScopeFunc 可以只给读根（那一侧就没有写边界了——慎用）。
func TestScopeFuncReadWriteSeparate(t *testing.T) {
	s := &Local{Scope: ScopeFunc(
		func() []string { return []string{"/r"} },
		nil,
	)}
	eff, err := s.effectiveScope(nil)
	if err != nil {
		t.Fatalf("effectiveScope: %v", err)
	}
	if len(eff.ReadRoots) != 1 || eff.ReadRoots[0] != "/r" {
		t.Fatalf("读根 = %v", eff.ReadRoots)
	}
	if len(eff.WriteRoots) != 0 {
		t.Fatalf("未给写根 = %v，应为空（空写根 = 写不了）", eff.WriteRoots)
	}
}
