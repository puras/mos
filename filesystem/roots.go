package filesystem

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 根来源与运行期可变根集合。
//
// 为什么需要它：宿主声明的根（FileAccessPolicy）是**静态数据**，但真实场景
// 里边界是会变的——用户在会话中途说"另外那个目录你也可以看"。此前框架只有
// 静态策略，宿主只能重建智能体（甚至重建进程）才能改边界；更糟的是"直接改
// slice"这条路：工具调用默认 4 路并发，改 slice 是数据竞争（race detector
// 会报），而且相对路径的锚点 = 第一个根，追加根会让锚点漂移。
//
// RootSet 就是把"边界可变"这件事做成**机制**：并发安全、锚点稳定、变更留痕。
// 框架只提供机制，"要不要加根、加哪个"始终是宿主的决定（模型没有这个权力）。

// RootProvider 是「根从哪来」的来源接口。
//
// 静态策略（*FileAccessPolicy）与运行期可变的根集合（*RootSet）都实现它；
// Sandboxed 只认这个接口——机制而非形态，宿主不必迁就框架的数据结构。
type RootProvider interface {
	// RootsFor 返回某类操作适用的根集合（绝对化、去重后的**副本**）。
	RootsFor(op Op) []string
}

// AnchorProvider 是可选接口：显式声明相对路径的锚点。
//
// 锚点是「相对路径的落点」：read("sub/a.txt") 里的 "sub/a.txt" 挂在
// 锚点下。默认锚点 = 该类操作的第一个根（既有行为）。
//
// 宿主显式声明锚点，是为了在**运行期加根**后仍让相对路径指向确定的目录：
// 锚点漂移比"新根只能用绝对路径访问"更危险——模型上一轮的相对路径引用会
// 悄悄指向别处，读到不该读的文件。
type AnchorProvider interface {
	// Anchor 返回锚点（绝对路径）；空串 = 用第一个根。
	Anchor() string
}

// RootChange 是一次根集合变更的留痕（加根/删根是安全事件：边界变了）。
type RootChange struct {
	// Op 是变更影响的操作类别（读/写）。
	Op Op
	// Action 是 "add" 或 "remove"。
	Action string
	// Root 是被变更的根（规范化后的绝对路径）。
	Root string
	// At 是变更时刻。
	At time.Time
}

// RootSet 是**运行期可变**的根集合：宿主可在智能体运行中增删根（典型场景：
// 用户在会话中授权另一个目录，智能体从"只有 A"变成"A + B"）。
//
// 三条纪律：
//
//   - **并发安全**：工具调用可并发（默认 4 路），根集合用 RWMutex 保护；
//     RootsFor 返回**快照副本**——调用方拿到的切片不受后续修改影响。
//   - **加根是追加，锚点不变**：锚点仍是第一个根（或宿主显式声明的锚点）。
//     换锚点请显式 SetAnchor——锚点漂移会让模型的相对路径悄悄指向别处。
//   - **留痕**：加根/删根记进 History。框架不替宿主写会话事件（事件目录是
//     预算制的，新增事件类型要有 PRD 依据），但"边界变了"这件事必须能被
//     事后审计，所以集合自己留一份变更历史。
//
// 零值可用但为空根（= 该类操作一律拒绝，fail-closed）。推荐用 NewRootSet。
type RootSet struct {
	mu      sync.RWMutex
	read    []string
	write   []string
	anchor  string
	history []RootChange
}

// NewRootSet 造一个根集合。readRoots / writeRoots 为空 = 该类操作一律拒绝
// （fail-closed：没有根就没有边界，不是"等于放开"）。
//
// 相对路径按进程当前目录绝对化——宿主应传绝对路径。
func NewRootSet(readRoots, writeRoots []string) *RootSet {
	s := &RootSet{}
	for _, r := range readRoots {
		s.read = append(s.read, normRoot(r))
	}
	for _, r := range writeRoots {
		s.write = append(s.write, normRoot(r))
	}
	s.read = dedupNonEmpty(s.read)
	s.write = dedupNonEmpty(s.write)
	return s
}

var _ RootProvider = (*RootSet)(nil)
var _ AnchorProvider = (*RootSet)(nil)

// RootsFor 返回该类操作适用的根集合（绝对化、去重的副本）。
func (s *RootSet) RootsFor(op Op) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if op == OpWrite {
		return absAll(s.write)
	}
	return absAll(s.read)
}

// Anchor 返回显式锚点；空串 = 用第一个根。
func (s *RootSet) Anchor() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.anchor
}

// SetAnchor 显式设置相对路径的锚点。空串 = 回到默认（第一个根）。
//
// 只在装配期或宿主认为必要时调用：锚点一变，模型手里的相对路径引用含义就变了。
func (s *RootSet) SetAnchor(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchor = normRoot(dir)
}

// AddRead 把根加进读根集合，返回**实际新增**的根（已存在的不重复计入）。
//
// 根不存在也照加：声明一个还不存在的目录只是暂时没有文件可访问，不是放宽。
func (s *RootSet) AddRead(roots ...string) []string {
	return s.add(OpRead, roots)
}

// AddWrite 把根加进写根集合，返回实际新增的根。
func (s *RootSet) AddWrite(roots ...string) []string {
	return s.add(OpWrite, roots)
}

// Add 把根同时加进读根与写根（宿主授权"读写整个目录"时的常用形状），
// 返回实际新增的根（读或写任一侧新增即计入）。
func (s *RootSet) Add(roots ...string) []string {
	addedRead := s.add(OpRead, roots)
	addedWrite := s.add(OpWrite, roots)
	return dedupNonEmpty(append(addedRead, addedWrite...))
}

// RemoveRead 把根从读根集合移除；返回它此前是否在集合里。
//
// 移除 = **收窄**边界（fail-closed 方向），随时可做。
func (s *RootSet) RemoveRead(root string) bool {
	return s.remove(OpRead, root)
}

// RemoveWrite 把根从写根集合移除；返回它此前是否在集合里。
func (s *RootSet) RemoveWrite(root string) bool {
	return s.remove(OpWrite, root)
}

// Remove 把根从读根与写根集合都移除；返回是否移除过。
func (s *RootSet) Remove(root string) bool {
	a := s.remove(OpRead, root)
	b := s.remove(OpWrite, root)
	return a || b
}

// Snapshot 返回当前根集合的一份**值拷贝**（给需要静态策略的场合用，
// 比如装配层打印横幅、或导出配置）。拷贝与集合此后互不影响。
func (s *RootSet) Snapshot() FileAccessPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return FileAccessPolicy{
		ReadRoots:  append([]string(nil), s.read...),
		WriteRoots: append([]string(nil), s.write...),
		Anchor:     s.anchor,
	}
}

// History 返回变更历史（副本，按时间顺序）。
func (s *RootSet) History() []RootChange {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]RootChange(nil), s.history...)
}

// add 往某一类根集合追加根，返回实际新增的根。
func (s *RootSet) add(op Op, roots []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var added []string
	for _, r := range roots {
		n := normRoot(r)
		if n == "" {
			continue
		}
		list := s.listFor(op)
		if containsStr(list, n) {
			continue
		}
		s.setListFor(op, append(list, n))
		added = append(added, n)
		s.history = append(s.history, RootChange{Op: op, Action: "add", Root: n, At: time.Now()})
	}
	return added
}

// remove 从某一类根集合移除根，返回是否移除过。
func (s *RootSet) remove(op Op, root string) bool {
	n := normRoot(root)
	if n == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.listFor(op)
	idx := indexStr(list, n)
	if idx < 0 {
		return false
	}
	s.setListFor(op, append(append([]string(nil), list[:idx]...), list[idx+1:]...))
	s.history = append(s.history, RootChange{Op: op, Action: "remove", Root: n, At: time.Now()})
	return true
}

// listFor 返回某一类根集合的底层切片（调用方须持锁）。
func (s *RootSet) listFor(op Op) []string {
	if op == OpWrite {
		return s.write
	}
	return s.read
}

// setListFor 覆盖某一类根集合（调用方须持锁）。
func (s *RootSet) setListFor(op Op, list []string) {
	if op == OpWrite {
		s.write = list
		return
	}
	s.read = list
}

// normRoot 把根规范化成绝对、干净的路径；空串/空白 → 空串（后续会被拒）。
func normRoot(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	a, err := filepath.Abs(s)
	if err != nil {
		return filepath.Clean(s)
	}
	return a
}

// dedupNonEmpty 去重并丢掉空串（保持顺序）。
func dedupNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || containsStr(out, s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// containsStr 判断切片里是否含某项。
func containsStr(list []string, s string) bool {
	return indexStr(list, s) >= 0
}

// indexStr 返回某项在切片里的下标（没有则 -1）。
func indexStr(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}
