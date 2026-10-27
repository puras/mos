package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 运行期加根：智能体初始只有 A，运行期加 B → B 从"越界"变成"可访问"。
//
// 这就是"运行时添加目录"的最小可验证形态：同一个 jail 实例，前后行为不同。
func TestRootSetAddAtRuntime(t *testing.T) {
	a := ws(t)
	b := ws(t)
	mustWrite(t, filepath.Join(b, "x.txt"), "hi")

	rs := NewRootSet([]string{a}, []string{a})
	fs := NewSandboxedWithRoots(rs)
	ctx := context.Background()

	// 加根之前：B 在边界外，读写都拒绝。
	if _, err := fs.Resolve(ctx, Request{Path: filepath.Join(b, "x.txt"), Op: OpRead}); err == nil {
		t.Fatal("加根前 B 应被拒绝")
	}

	// 运行期加根（宿主行为：用户授权了 B）。
	rs.Add(b)

	// 加根之后：立刻生效（边界每次调用现判）。
	if _, err := fs.Resolve(ctx, Request{Path: filepath.Join(b, "x.txt"), Op: OpRead}); err != nil {
		t.Fatalf("加根后 B 应可读: %v", err)
	}
	if _, err := fs.Resolve(ctx, Request{Path: filepath.Join(b, "y.txt"), Op: OpWrite}); err != nil {
		t.Fatalf("加根后 B 应可写: %v", err)
	}
}

// 只加读根不授予写权限（读写的授权是分开的两件事）。
func TestRootSetReadWriteSeparate(t *testing.T) {
	a := ws(t)
	b := ws(t)

	rs := NewRootSet([]string{a}, []string{a})
	rs.AddRead(b)
	fs := NewSandboxedWithRoots(rs)
	ctx := context.Background()

	if _, err := fs.Resolve(ctx, Request{Path: filepath.Join(b, "x.txt"), Op: OpRead}); err != nil {
		t.Fatalf("加读根后应可读: %v", err)
	}
	if _, err := fs.Resolve(ctx, Request{Path: filepath.Join(b, "x.txt"), Op: OpWrite}); err == nil {
		t.Fatal("只加读根不应获得写权限")
	}
}

// 移除根 = 收窄边界：立刻生效。
func TestRootSetRemoveNarrows(t *testing.T) {
	a := ws(t)
	b := ws(t)
	rs := NewRootSet([]string{a, b}, []string{a, b})
	fs := NewSandboxedWithRoots(rs)
	ctx := context.Background()

	if _, err := fs.Resolve(ctx, Request{Path: filepath.Join(b, "x.txt"), Op: OpRead}); err != nil {
		t.Fatalf("移除前 B 应可读: %v", err)
	}
	rs.Remove(b)
	if _, err := fs.Resolve(ctx, Request{Path: filepath.Join(b, "x.txt"), Op: OpRead}); err == nil {
		t.Fatal("移除后 B 应被拒绝")
	}
}

// 加根是**追加**：锚点（相对路径的落点）不变——模型的相对路径引用不会因为
// 加了一个根就悄悄指向别处。
func TestRootSetAnchorStableOnAdd(t *testing.T) {
	a := ws(t)
	b := ws(t)
	mustWrite(t, filepath.Join(a, "rel.txt"), "x")
	mustWrite(t, filepath.Join(b, "rel.txt"), "y")

	rs := NewRootSet([]string{a}, []string{a})
	fs := NewSandboxedWithRoots(rs)
	ctx := context.Background()

	rs.Add(b)
	res, err := fs.Resolve(ctx, Request{Path: "rel.txt", Op: OpRead})
	if err != nil {
		t.Fatalf("相对路径应放行: %v", err)
	}
	if res.TargetKey != filepath.Join(a, "rel.txt") {
		t.Fatalf("锚点应仍是第一个根 %q，得到 %q", a, res.TargetKey)
	}
}

// 宿主可显式指定锚点：加根后仍让相对路径指向确定目录。
//
// 注意：锚点只是相对路径的**落点**，解析结果仍须落在允许根内——锚点不是
// 逃逸口（锚点在根外 → 相对路径一律被拒）。
func TestRootSetExplicitAnchor(t *testing.T) {
	a := ws(t)
	b := ws(t)
	mustWrite(t, filepath.Join(b, "rel.txt"), "y")

	rs := NewRootSet([]string{a}, []string{a})
	rs.Add(b)
	rs.SetAnchor(b)
	fs := NewSandboxedWithRoots(rs)

	res, err := fs.Resolve(context.Background(), Request{Path: "rel.txt", Op: OpRead})
	if err != nil {
		t.Fatalf("相对路径应放行: %v", err)
	}
	if res.TargetKey != filepath.Join(b, "rel.txt") {
		t.Fatalf("显式锚点应生效，得到 %q", res.TargetKey)
	}
}

// 锚点在允许根之外 = 相对路径一律被拒（锚点不是逃逸口）。
func TestRootSetAnchorOutsideRootsDenies(t *testing.T) {
	a := ws(t)
	outside := ws(t)
	rs := NewRootSet([]string{a}, []string{a})
	rs.SetAnchor(outside)
	fs := NewSandboxedWithRoots(rs)
	if _, err := fs.Resolve(context.Background(), Request{Path: "rel.txt", Op: OpRead}); err == nil {
		t.Fatal("锚点在根外时相对路径应被拒绝")
	}
}

// 快照是值拷贝：之后改集合，快照不变（宿主拿它导出配置/打印横幅）。
func TestRootSetSnapshotIsCopy(t *testing.T) {
	a := ws(t)
	rs := NewRootSet([]string{a}, []string{a})
	snap := rs.Snapshot()
	rs.Add("/tmp/other")
	if len(snap.ReadRoots) != 1 {
		t.Fatalf("快照不应被后续修改影响: %v", snap.ReadRoots)
	}
	if snap.ReadRoots[0] != a {
		t.Fatalf("快照根 = %v，want %s", snap.ReadRoots, a)
	}
}

// RootsFor 返回副本：调用方改了返回的切片，不影响集合。
func TestRootSetRootsForReturnsCopy(t *testing.T) {
	a := ws(t)
	rs := NewRootSet([]string{a}, []string{a})
	got := rs.RootsFor(OpRead)
	if len(got) != 1 {
		t.Fatalf("读根 = %v", got)
	}
	got[0] = "/hacked"
	if again := rs.RootsFor(OpRead); again[0] != a {
		t.Fatalf("集合被外部改了: %v", again)
	}
}

// 变更留痕：加根/删根是安全事件，事后要能审计。
func TestRootSetHistory(t *testing.T) {
	a := ws(t)
	rs := NewRootSet([]string{a}, []string{a})
	rs.Add("/tmp/b")
	rs.Remove("/tmp/b")
	h := rs.History()
	if len(h) != 4 {
		t.Fatalf("历史条数 = %d，want 4（读/写各一条）: %v", len(h), h)
	}
	if h[0].Action != "add" || h[0].Op != OpRead {
		t.Fatalf("第一条历史 = %+v", h[0])
	}
	if h[1].Action != "add" || h[1].Op != OpWrite {
		t.Fatalf("第二条历史 = %+v", h[1])
	}
	if h[2].Action != "remove" || h[3].Action != "remove" {
		t.Fatalf("后两条历史 = %+v %+v", h[2], h[3])
	}
}

// 并发安全：多路加根/删根与解析同时进行（-race 下能抓出数据竞争）。
func TestRootSetConcurrentSafe(t *testing.T) {
	a := ws(t)
	rs := NewRootSet([]string{a}, []string{a})
	fs := NewSandboxedWithRoots(rs)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := filepath.Join(t.TempDir(), "d")
			for j := 0; j < 20; j++ {
				rs.Add(dir)
				rs.Remove(dir)
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = fs.Resolve(ctx, Request{Path: "x.txt", Op: OpRead})
				_ = rs.Snapshot()
				_ = rs.History()
			}
		}()
	}
	wg.Wait()
}

// 没有根来源 = 一律拒绝（fail-closed）。
func TestSandboxedWithNilRootsDeniesAll(t *testing.T) {
	fs := NewSandboxedWithRoots(nil)
	if _, err := fs.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpRead}); err == nil {
		t.Fatal("nil 根来源应拒绝一切")
	}
	var denied *PathDeniedError
	_, err := fs.Resolve(context.Background(), Request{Path: "/etc/passwd", Op: OpRead})
	if !errors.As(err, &denied) {
		t.Fatalf("应返回 *PathDeniedError，得到 %T", err)
	}
}

// 拒绝理由里带上"当前允许的根"：模型据此能自己改用绝对路径重试。
func TestDeniedErrorCarriesCurrentRoots(t *testing.T) {
	a := ws(t)
	b := ws(t)
	rs := NewRootSet([]string{a}, []string{a})
	fs := NewSandboxedWithRoots(rs)

	rs.Add(b)
	_, err := fs.Resolve(context.Background(), Request{Path: filepath.Join(ws(t), "z.txt"), Op: OpRead})
	var denied *PathDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("应返回 *PathDeniedError，得到 %T", err)
	}
	found := false
	for _, r := range denied.AllowedRoots {
		if r == b {
			found = true
		}
	}
	if !found {
		t.Fatalf("拒绝理由应带上当前的根 %v，实际 %v", b, denied.AllowedRoots)
	}
}

// 根不存在也照加：声明一个还不存在的目录只是暂时没有文件可访问，不是放宽。
func TestRootSetAddNonExistentDir(t *testing.T) {
	a := ws(t)
	missing := filepath.Join(t.TempDir(), "not-yet")
	rs := NewRootSet([]string{a}, []string{a})
	added := rs.Add(missing)
	if len(added) != 1 {
		t.Fatalf("新增根 = %v", added)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("测试前提不成立：目录不该被创建")
	}
}
