package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/tool"
)

// Edit 精确替换文件中的文本片段——改代码类任务的正解。
//
// 为什么需要它（对照 write 的整文件重写）：
//   - 整文件重写要求模型**重新产出整个文件**，它既怕改错别处、又无法确认自己
//     改对了没，于是反复重写，容易空转；
//   - 精确替换只要求模型给出**要改的那几行**，改动的意图与结果一一对应。
//
// 语义参照 DSH 的 edit（packages/fs/tool-fs/src/edit.ts + fs-local/src/fsio.ts）：
//   - **唯一性**：old_string 必须在文件里恰好出现一次；出现多次 → 报错并报出
//     次数，让模型补足上下文（或显式要求 replace_all）。宁可报错也不静默改多处；
//   - **行尾归一**：匹配前把 old_string 的 CRLF 归一为 LF，写回时按原文件的
//     行尾还原——否则 Windows 文件里模型给的 \n 永远匹配不上；
//   - **BOM 保留**：匹配前剥掉 UTF-8 BOM，写回时还原（模型看不见 BOM）；
//   - **原子写**：先写同目录临时文件再 rename 覆盖，写一半失败不会留下半个文件；
//   - **拒绝二进制**：含 NUL 字节的文件不是文本，不做编辑。
//
// 与 write 的分工：edit 改已有文件（精确），write 写整份内容（新建或
// 整体覆盖）。edit 不创建文件——文件不存在就报错。
type Edit struct {
	FS filesystem.FileSystem
	// Gate 是写前观察门（可选，nil = 不启用）：编辑前必须先读过，且读后
	// 文件没变（S5h 防丢失更新）。
	Gate *ObserveGate
}

// NewEdit 构造编辑工具。
func NewEdit(fsys filesystem.FileSystem) *Edit { return &Edit{FS: fsys} }

// NewEditObserved 构造带观察门的编辑工具（S5h）。
func NewEditObserved(fsys filesystem.FileSystem, gate *ObserveGate) *Edit {
	return &Edit{FS: fsys, Gate: gate}
}

type editParams struct {
	Path  string       `json:"path" mos:"desc=文件路径（相对路径以工作目录为锚点）"`
	Edits []editChange `json:"edits" mos:"desc=一处或多处替换；每处的 old_string 必须在文件里唯一出现"`
}

// （editParams 只用于生成 JSON Schema；实际解析走 editArgs + decodeEdits，
//  以容忍模型把 edits 发成单个对象或 JSON 字符串。）

// editChange 是一处精确替换（PI 的 edits[] 元素）。
type editChange struct {
	OldString string `json:"old_string" mos:"desc=要被替换的原文本，必须在文件里唯一出现"`
	NewString string `json:"new_string" mos:"desc=替换后的新文本（空字符串表示删除该片段）"`
}

func (Edit) Name() string { return "edit" }

func (Edit) Description() string {
	// 措辞参照 PI 的 edit 工具说明（其 promptGuidelines 明确要求"一次调用改多处"）。
	return "精确替换文件中的文本（受工作目录沙箱限制）。" +
		"改多处请在同一次调用里给出多个 edits——每处的 old_string 必须在文件里唯一出现，" +
		"且各处的 old_string 互不重叠（改同一片区域就合并成一条）。" +
		"所有 old_string 都按**原文件**匹配，不是按前一条改完的结果。" +
		"改代码优先用本工具，而不是 write 整文件重写。"
}

func (Edit) Parameters() any { return &editParams{} }

// editArgs 是宽松的入参形状：edits 先按原始 JSON 收下，再自己归一
// （模型可能把它发成数组 / 单个对象 / JSON 字符串）。
type editArgs struct {
	Path  string          `json:"path"`
	Edits json.RawMessage `json:"edits"`
}

// Execute 实现 tool.Tool.Execute。
func (e *Edit) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[editArgs](args)
	if err != nil {
		return nil, err
	}
	changes, err := decodeEdits(p.Edits)
	if err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path 不能为空")
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf("edits 不能为空")
	}
	for i, c := range changes {
		if c.OldString == "" {
			return nil, fmt.Errorf("第 %d 处编辑的 old_string 不能为空", i+1)
		}
		if c.OldString == c.NewString {
			return nil, fmt.Errorf("第 %d 处编辑的 old_string 与 new_string 相同，无需编辑", i+1)
		}
	}
	if e.FS == nil {
		return nil, &filesystem.NotConfiguredError{}
	}
	// 写操作走写 jail（与 write 同一关卡）。
	res, err := e.FS.Resolve(ctx, filesystem.Request{Path: p.Path, Op: filesystem.OpWrite})
	if err != nil {
		return nil, err
	}
	// 写前观察（S5h）：编辑前必须先读过，且读后文件没被别人改过。
	if err := e.Gate.CheckEdit(res.TargetKey, res.DisplayPath); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(res.TargetKey)
	if err != nil {
		return nil, fmt.Errorf("读取文件 %q 失败: %w", res.DisplayPath, err)
	}
	if strings.IndexByte(string(raw), 0) >= 0 {
		return nil, fmt.Errorf("不能编辑 %q：二进制文件", res.DisplayPath)
	}
	bom, content := splitBOM(string(raw))
	ending := detectLineEndings(content)
	content = normalizeLineEndings(content)

	// 全部按**原文**定位（PI 的规则：每处 oldText 都对原文件匹配，不是增量匹配）。
	// 先定位再统一应用，顺序不影响结果。
	type located struct {
		start, end int
		change     editChange
	}
	locs := make([]located, 0, len(changes))
	for i, c := range changes {
		old := normalizeLineEndings(c.OldString)
		count := strings.Count(content, old)
		if count == 0 {
			return nil, fmt.Errorf("第 %d 处编辑的 old_string 在 %q 里没有找到", i+1, res.DisplayPath)
		}
		if count > 1 {
			// 唯一性即安全：宁可报错让模型补足上下文，也不静默改多处（PI 的规则）。
			return nil, fmt.Errorf("第 %d 处编辑的 old_string 在 %q 里出现了 %d 次：请提供更具体的上下文（保持唯一）",
				i+1, res.DisplayPath, count)
		}
		start := strings.Index(content, old)
		locs = append(locs, located{start: start, end: start + len(old), change: c})
	}
	// 不重叠检查（PI：改同一片区域就合并成一条）。
	for i := 1; i < len(locs); i++ {
		if locs[i].start < locs[i-1].end {
			return nil, fmt.Errorf("第 %d 处编辑与第 %d 处重叠：请合并成一条 edit", i+1, i)
		}
	}
	// 按位置从后往前替换，避免前面的替换让后面的偏移失效。
	sorted := make([]located, len(locs))
	copy(sorted, locs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	updated := content
	for i := len(sorted) - 1; i >= 0; i-- {
		l := sorted[i]
		newText := normalizeLineEndings(l.change.NewString)
		updated = updated[:l.start] + newText + updated[l.end:]
	}
	firstLine := 1 + strings.Count(content[:sorted[0].start], "\n")

	// 还原行尾与 BOM，再原子写回。
	final := bom + restoreLineEndings(updated, ending)
	if err := atomicWrite(res.TargetKey, []byte(final), 0o644); err != nil {
		return nil, fmt.Errorf("写入文件 %q 失败: %w", res.DisplayPath, err)
	}
	// 编辑成功 = 新的权威状态（S5h）：记录新版本，后续 edit 以此为 CAS 基准。
	if v, present, err := fileVersion(res.TargetKey); err == nil && present {
		e.Gate.Record(res.TargetKey, v, true)
	}
	return map[string]any{
		"path":               res.DisplayPath,
		"replacements":       len(sorted),
		"first_changed_line": firstLine,
		"bytes":              len(final),
	}, nil
}

// decodeEdits 归一模型的 edits 形状：PI 的实战经验——有些模型把 edits 发成
// 单个对象或 JSON 字符串，而不是数组。与其报错让模型重试，不如直接收下。
func decodeEdits(raw json.RawMessage) ([]editChange, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		// 整个数组被塞进了一个字符串里
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("edits 解析失败: %w", err)
		}
		return decodeEdits(json.RawMessage(strings.TrimSpace(s)))
	}
	if strings.HasPrefix(trimmed, "{") {
		var one editChange
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("edits 解析失败: %w", err)
		}
		return []editChange{one}, nil
	}
	var many []editChange
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("edits 解析失败: %w", err)
	}
	return many, nil
}

var _ tool.Tool = (*Edit)(nil)

// atomicWrite 原子写：先写同目录临时文件，再 rename 覆盖目标。
// 同目录保证 rename 在同一文件系统内（跨文件系统的 rename 不是原子的）；
// 写一半失败时目标文件保持原样，不会留下半个文件。
func atomicWrite(target string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".mos-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // rename 成功后这里是无操作
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// splitBOM 剥掉开头的 UTF-8 BOM，返回 (BOM, 正文)。
// 模型看不见 BOM，它给出的 old_string 不含 BOM——匹配时必须剥掉。
func splitBOM(s string) (bom, rest string) {
	const bomMark = "\ufeff"
	if strings.HasPrefix(s, bomMark) {
		return bomMark, s[len(bomMark):]
	}
	return "", s
}

// detectLineEndings 判定原文的行尾风格（只看第一个换行符）。
func detectLineEndings(s string) string {
	if strings.Contains(s, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// normalizeLineEndings 把 CRLF 归一为 LF（匹配与比较都在归一后的文本上进行）。
func normalizeLineEndings(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// restoreLineEndings 按原文件的行尾风格还原。
func restoreLineEndings(s, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	return s
}
