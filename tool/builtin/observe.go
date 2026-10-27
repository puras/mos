package builtin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
)

// ObserveGate 是**写前观察**门（S5h）：记录「模型见过的文件状态」，
// 并在写/改之前做新鲜度校验。
//
// 要解决的问题：模型 read 一个文件之后，文件被别人（shell、另一个会话、人）
// 改了，模型再 write/edit 就会**盖掉它没见过的改动**（丢失更新）。
//
// 契约（对齐 DSH 的 fs-observation-policy）：
//   - **不许覆盖你没见过的东西**：write 到一个没读过的已有文件 → 拒绝；
//   - **编辑前必须先读**：edit 一个没读过的文件 → 拒绝；
//   - **读与写之间文件变了 → 拒绝**（CAS：按观察到的版本校验）——模型被迫
//     重读，看到真实状态后再决定。这条是核心价值：shell 改了文件 → edit
//     被拒 → 模型重读，丢失更新在发生前被拦下。
//
// **新建文件不需要先读**（没见过 → createIfAbsent）：正常创建流程不受影响。
//
// 并发与边界（诚实声明）：
//   - 进程内：write/edit 是独占执行（execpath 的屏障调度），"校验 → 写 →
//     记录"窗口天然串行，本门不需要额外的每文件锁；
//   - 跨进程：不设防——两个进程同时写同一文件仍可能竞争（DSH 用 hard-link
//     no-replace 解决 createIfAbsent 的跨进程原子性，此处暂未做）。
type ObserveGate struct {
	mu   sync.Mutex
	seen map[string]observation // key: 目标路径（Resolved.TargetKey）
}

// observation 是一次权威观察：文件存在与否 + 当时的版本令牌。
type observation struct {
	present bool
	version string
}

// NewObserveGate 创建观察门。nil 门 = 不启用写前观察（工具行为同旧版）。
func NewObserveGate() *ObserveGate {
	return &ObserveGate{seen: make(map[string]observation)}
}

// CheckWrite 校验一次写入。返回 error 时**不得写入**（理由回填给模型）。
func (g *ObserveGate) CheckWrite(targetKey, display string) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	rec, seen := g.seen[targetKey]
	g.mu.Unlock()

	cur, present, err := fileVersion(targetKey)
	if err != nil {
		return fmt.Errorf("探测文件状态失败: %w", err)
	}
	if present && !isRegular(targetKey) {
		return fmt.Errorf("write 拒绝：%q 不是常规文件", display)
	}

	switch {
	case !seen:
		// 没见过：createIfAbsent——已存在就是"盲覆盖"，先读再写。
		if present {
			return fmt.Errorf("write 拒绝：%q 已存在但未被读取过。先 read 读取当前内容，再决定是否覆盖", display)
		}
	case rec.present:
		// 见过且存在：replaceIfVersion——按观察到的版本做 CAS。
		if !present {
			return fmt.Errorf("write 拒绝：%q 自读取后已被删除", display)
		}
		if cur != rec.version {
			return fmt.Errorf("write 拒绝：%q 自读取后已被修改。请重新 read 获取最新内容后再写入", display)
		}
	default:
		// 见过但当时不存在：现在却存在了——它是在观察之后出现的，内容未知。
		if present {
			return fmt.Errorf("write 拒绝：%q 在读取时不存在、现在却存在了。请重新 read 确认内容后再决定", display)
		}
	}
	return nil
}

// CheckEdit 校验一次编辑。返回 error 时**不得编辑**（理由回填给模型）。
func (g *ObserveGate) CheckEdit(targetKey, display string) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	rec, seen := g.seen[targetKey]
	g.mu.Unlock()

	cur, present, err := fileVersion(targetKey)
	if err != nil {
		return fmt.Errorf("探测文件状态失败: %w", err)
	}

	switch {
	case !seen:
		return fmt.Errorf("edit 拒绝：编辑前必须先 read 读取 %q 的当前内容", display)
	case !rec.present:
		return fmt.Errorf("edit 拒绝：%q 不存在（此前观察到它不存在；如需创建请用 write）", display)
	case !present:
		return fmt.Errorf("edit 拒绝：%q 自读取后已被删除", display)
	case cur != rec.version:
		return fmt.Errorf("edit 拒绝：%q 自读取后已被修改。请重新 read 获取最新内容后再编辑", display)
	}
	return nil
}

// Record 记录一次权威观察（读到的内容、写入后的新状态）。
func (g *ObserveGate) Record(targetKey, version string, present bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen[targetKey] = observation{present: present, version: version}
}

// ObserveRead 供 read 调用：读成功记"存在"，文件不存在记"不存在"。
// 其余错误（无权限等）不记录——那不是对文件存在性的权威回答。
func (g *ObserveGate) ObserveRead(targetKey string) {
	if g == nil {
		return
	}
	version, present, err := fileVersion(targetKey)
	if err != nil {
		return
	}
	g.Record(targetKey, version, present)
}

// fileVersion 生成版本令牌：dev:ino:size:mtimeNs:ctimeNs（DSH 同款——
// 文件身份 + 新鲜度元数据）。present=false 表示文件不存在。
//
// 非 unix 平台拿不到 dev/ino 时退化为 size:mtimeNs（弱一些：同尺寸同刻的
// 写入不可区分——文档已注明）。
func fileVersion(path string) (version string, present bool, err error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	v, ok := versionOf(info)
	return v, ok, nil
}

// versionOf 从已有的 FileInfo 取版本令牌（避免二次 stat）。
// 平台专属的身份提取见 observe_version_*.go（unix 拿 dev/ino/ctime，
// 其他平台退化为 size:mtime）。
func versionOf(info os.FileInfo) (version string, present bool) {
	v, ok := fileIdentity(info)
	if !ok {
		return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano()), true
	}
	return v, true
}

// isRegular 判断目标是否常规文件（目录/设备文件不该被 write 覆盖）。
func isRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
