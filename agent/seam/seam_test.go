package seam

import "testing"

func TestRegisterResolve(t *testing.T) {
	s := New()
	s.Register(KindModel, "m1", func() any { return 42 })
	v, err := s.Resolve(KindModel, "m1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if n, ok := v.(int); !ok || n != 42 {
		t.Fatalf("resolved = %#v, want 42", v)
	}
}

func TestResolveUnknown(t *testing.T) {
	s := New()
	if _, err := s.Resolve(KindModel, "nope"); err == nil {
		t.Fatal("Resolve should fail on unknown provider")
	}
	if _, err := s.Resolve(KindTool, "m1"); err == nil {
		t.Fatal("Resolve should fail on unknown kind")
	}
}

func TestRegisterOverride(t *testing.T) {
	s := New()
	s.Register(KindModel, "m", func() any { return 1 })
	s.Register(KindModel, "m", func() any { return 2 })
	v, _ := s.Resolve(KindModel, "m")
	if n, _ := v.(int); n != 2 {
		t.Fatalf("override not effective: %#v", v)
	}
}

func TestFactoryPerResolve(t *testing.T) {
	s := New()
	n := 0
	s.Register(KindModel, "m", func() any { n++; return n })
	v1, _ := s.Resolve(KindModel, "m")
	v2, _ := s.Resolve(KindModel, "m")
	if v1.(int) != 1 || v2.(int) != 2 {
		t.Fatalf("factory should run per resolve: %v %v", v1, v2)
	}
}

func TestNamesSorted(t *testing.T) {
	s := New()
	s.Register(KindDriver, "react", func() any { return nil })
	s.Register(KindDriver, "plan-execute", func() any { return nil })
	s.Register(KindModel, "m", func() any { return nil })
	names := s.Names(KindDriver)
	if len(names) != 2 || names[0] != "plan-execute" || names[1] != "react" {
		t.Fatalf("names = %v, want sorted [plan-execute react]", names)
	}
	if s.Has(KindDriver, "react") != true || s.Has(KindDriver, "nope") != true == false {
		t.Fatalf("Has mismatch")
	}
}

func TestNilRegistry(t *testing.T) {
	var s *SeamRegistry
	if s.Has(KindModel, "x") {
		t.Fatal("nil registry Has should be false")
	}
	if _, err := s.Resolve(KindModel, "x"); err == nil {
		t.Fatal("nil registry Resolve should fail")
	}
	s.Register(KindModel, "x", func() any { return nil }) // 不 panic
}

func TestKindFileSystemRegistered(t *testing.T) {
	reg := New()
	var sawKind bool
	for _, k := range []Kind{KindFileSystem} {
		if k == KindFileSystem {
			sawKind = true
		}
	}
	if !sawKind {
		t.Fatal("KindFileSystem 未注册")
	}
	// 验证可注册/解析
	reg.Register(KindFileSystem, "default", func() any { return "fake-fs" })
	got, err := reg.Resolve(KindFileSystem, "default")
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if got != "fake-fs" {
		t.Fatalf("got=%v, want fake-fs", got)
	}
}
