package executor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunOrderAndIntercept 验证三阶段：拦截集合直接回填、允许集合并发执行、
// 结果按输入顺序回填（与输入等长同序）。
func TestRunOrderAndIntercept(t *testing.T) {
	items := []Item[int]{
		{Input: 1},
		{Input: 2},
		{Input: 3}, // preflight 拦截（偶数拒绝）
		{Input: 4},
	}
	preflight := func(_ context.Context, v int) (bool, error) {
		if v%2 == 0 {
			return false, errors.New("denied")
		}
		return true, nil
	}
	execute := func(_ context.Context, v int) (string, error) {
		return "r" + string(rune('0'+v)), nil
	}

	outs := Run(context.Background(), items, 0, preflight, execute)
	if len(outs) != 4 {
		t.Fatalf("len = %d, want 4", len(outs))
	}
	for i, want := range []struct {
		denied bool
		output string
		err    bool
	}{
		{false, "r1", false},
		{true, "", true},
		{false, "r3", false},
		{true, "", true},
	} {
		o := outs[i]
		if o.Denied != want.denied || o.Output != want.output || (o.Err != nil) != want.err {
			t.Fatalf("outs[%d] = %+v, want denied=%v output=%q err=%v", i, o, want.denied, want.output, want.err)
		}
	}
	// 输入顺序保持（Denied 条目 Input 原样）
	if outs[1].Input != 2 || outs[3].Input != 4 {
		t.Fatalf("input order broken: %+v", outs)
	}
}

// TestRunParallel 验证允许集合并发执行（全部启动后才有完成，证明并行）+ 有界并发。
func TestRunParallel(t *testing.T) {
	items := make([]Item[int], 8)
	for i := range items {
		items[i] = Item[int]{Input: i}
	}
	preflight := func(_ context.Context, v int) (bool, error) { return true, nil }

	var active, maxActive int64
	var mu sync.Mutex
	execute := func(ctx context.Context, v int) (int, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return v * 2, nil
	}

	start := time.Now()
	outs := Run(context.Background(), items, 3, preflight, execute)
	elapsed := time.Since(start)

	if maxActive > 3 {
		t.Fatalf("max active = %d, want <= 3", maxActive)
	}
	if maxActive < 2 {
		t.Fatalf("max active = %d, want > 1（未并发）", maxActive)
	}
	// 8 条 × 20ms / 并发 3 ≈ 60ms；串行需 160ms。宽松断言防止 CI 抖动。
	if elapsed > 120*time.Millisecond {
		t.Fatalf("elapsed = %v, want < 120ms（应并发执行）", elapsed)
	}
	for i, o := range outs {
		if o.Output != i*2 {
			t.Fatalf("outs[%d].Output = %d, want %d", i, o.Output, i*2)
		}
	}
}

// TestRunSequentialFallback 验证任一条目 Sequential=true 时整批串行回退
// （执行时间 ≈ 串行总和，且完成顺序 = 输入顺序）。
func TestRunSequentialFallback(t *testing.T) {
	items := []Item[int]{
		{Input: 1},
		{Input: 2, Sequential: true}, // 强制整批串行
		{Input: 3},
	}
	preflight := func(_ context.Context, v int) (bool, error) { return true, nil }

	var order []int
	var mu sync.Mutex
	execute := func(ctx context.Context, v int) (int, error) {
		time.Sleep(15 * time.Millisecond)
		mu.Lock()
		order = append(order, v)
		mu.Unlock()
		return v, nil
	}

	start := time.Now()
	outs := Run(context.Background(), items, 4, preflight, execute)
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= 40ms（应串行）", elapsed)
	}
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("execution order = %v, want [1 2 3]（串行保序）", order)
	}
	_ = outs
}

// TestRunContextCancel 验证 ctx 取消传播：未启动条目直接回填 ctx.Err()。
func TestRunContextCancel(t *testing.T) {
	items := make([]Item[int], 6)
	for i := range items {
		items[i] = Item[int]{Input: i}
	}
	preflight := func(_ context.Context, v int) (bool, error) { return true, nil }

	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	execute := func(c context.Context, v int) (int, error) {
		started.Add(1)
		time.Sleep(50 * time.Millisecond)
		<-c.Done()
		return v, c.Err()
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	outs := Run(ctx, items, 2, preflight, execute)

	// 至少 1 条已启动；未启动的条目必须回填 ctx.Err()
	if started.Load() == 0 {
		t.Fatalf("no execute started")
	}
	var canceled int
	for _, o := range outs {
		if errors.Is(o.Err, context.Canceled) {
			canceled++
		}
	}
	if canceled == 0 {
		t.Fatalf("no canceled outcome, got %+v", outs)
	}
}

// TestRunEmptyAndSingle 验证空输入与单条目边界。
func TestRunEmptyAndSingle(t *testing.T) {
	if outs := Run[int, int](context.Background(), nil, 0,
		func(context.Context, int) (bool, error) { return true, nil },
		func(context.Context, int) (int, error) { return 0, nil }); outs != nil {
		t.Fatalf("empty = %+v, want nil", outs)
	}

	outs := Run(context.Background(), []Item[int]{{Input: 7}}, 0,
		func(context.Context, int) (bool, error) { return true, nil },
		func(context.Context, int) (int, error) { return 49, nil })
	if len(outs) != 1 || outs[0].Output != 49 || outs[0].Denied {
		t.Fatalf("single = %+v", outs)
	}
}
