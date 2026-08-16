package scope

import (
	"sync"
	"testing"
)

// TestCloseReverseOrder 验证 Close 按注册逆序回滚。
func TestCloseReverseOrder(t *testing.T) {
	s := New()
	var order []string
	s.On(func() { order = append(order, "a") })
	s.On(func() { order = append(order, "b") })
	s.On(func() { order = append(order, "c") })
	s.Close()
	if len(order) != 3 || order[0] != "c" || order[1] != "b" || order[2] != "a" {
		t.Fatalf("order = %v, want [c b a]", order)
	}
}

// TestDisposer 验证单独撤销：只移除对应注册项。
func TestDisposer(t *testing.T) {
	s := New()
	var fired []string
	d1 := s.On(func() { fired = append(fired, "a") })
	s.On(func() { fired = append(fired, "b") })
	d1()
	s.Close()
	if len(fired) != 1 || fired[0] != "b" {
		t.Fatalf("fired = %v, want [b]", fired)
	}
}

// TestCloseIdempotent 验证 Close 幂等。
func TestCloseIdempotent(t *testing.T) {
	s := New()
	n := 0
	s.On(func() { n++ })
	s.Close()
	s.Close()
	if n != 1 {
		t.Fatalf("n = %d, want 1", n)
	}
}

// TestOnAfterClose 验证 Close 后再注册立即执行。
func TestOnAfterClose(t *testing.T) {
	s := New()
	n := 0
	s.Close()
	s.On(func() { n++ })
	if n != 1 {
		t.Fatalf("n = %d, want 1（关闭后注册立即执行）", n)
	}
}

// TestScopeConcurrent 验证并发注册/关闭无数据竞争。
func TestScopeConcurrent(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.On(func() {})
		}()
	}
	wg.Wait()
	s.Close()
}
