package contextfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// writeFile 写一个文件（自动建父目录）。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录 %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件 %q: %v", path, err)
	}
}

func TestLoadPicksFirstCandidateInDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "agents 内容")
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "claude 内容")

	res, err := Load(dir, Options{MaxDepth: 0})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("文件数 = %d，want 1", len(res.Files))
	}
	if got, want := filepath.Base(res.Files[0].Path), "AGENTS.md"; got != want {
		t.Fatalf("命中文件 = %q，want %q", got, want)
	}
	if !strings.Contains(res.Text, "agents 内容") {
		t.Fatalf("正文缺少内容：%s", res.Text)
	}
	if strings.Contains(res.Text, "claude 内容") {
		t.Fatalf("同目录的 CLAUDE.md 不该被一并加载：%s", res.Text)
	}
}

func TestLoadOverrideShadowsSameDirectoryOnly(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "根目录约定")
	writeFile(t, filepath.Join(child, "AGENTS.override.md"), "子目录覆盖")
	writeFile(t, filepath.Join(child, "AGENTS.md"), "子目录普通约定")

	res, err := Load(child, Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("文件数 = %d，want 2（根目录 AGENTS.md + 子目录 override）", len(res.Files))
	}
	// 顺序：外层在前、内层在后。
	if got, want := filepath.Base(res.Files[0].Path), "AGENTS.md"; got != want {
		t.Fatalf("第一个文件 = %q，want %q（外层约定在前）", got, want)
	}
	if !strings.Contains(res.Files[0].Content, "根目录约定") {
		t.Fatalf("第一个文件内容不对：%q", res.Files[0].Content)
	}
	if got, want := filepath.Base(res.Files[1].Path), "AGENTS.override.md"; got != want {
		t.Fatalf("第二个文件 = %q，want %q（同目录内 override 替换普通约定）", got, want)
	}
	if strings.Contains(res.Text, "子目录普通约定") {
		t.Fatalf("同目录的普通 AGENTS.md 应被 override 替换：%s", res.Text)
	}
}

func TestLoadOrderIsGlobalThenOuterToInner(t *testing.T) {
	global := t.TempDir()
	root := t.TempDir()
	child := filepath.Join(root, "child")
	writeFile(t, filepath.Join(global, "AGENTS.md"), "全局约定")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "外层约定")
	writeFile(t, filepath.Join(child, "AGENTS.md"), "内层约定")

	res, err := Load(child, Options{MaxDepth: 1, GlobalDirs: []string{global}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 3 {
		t.Fatalf("文件数 = %d，want 3", len(res.Files))
	}
	want := []string{"全局约定", "外层约定", "内层约定"}
	for i, w := range want {
		if !strings.Contains(res.Files[i].Content, w) {
			t.Fatalf("第 %d 个文件 = %q，want 含 %q", i, res.Files[i].Content, w)
		}
	}
	// 渲染后的正文顺序与文件顺序一致。
	idxGlobal := strings.Index(res.Text, "全局约定")
	idxOuter := strings.Index(res.Text, "外层约定")
	idxInner := strings.Index(res.Text, "内层约定")
	if !(idxGlobal < idxOuter && idxOuter < idxInner) {
		t.Fatalf("正文顺序不对：global=%d outer=%d inner=%d", idxGlobal, idxOuter, idxInner)
	}
}

func TestLoadDeduplicatesSymlinkedFile(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "同一份约定")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	link := filepath.Join(child, "AGENTS.md")
	if err := os.Symlink(filepath.Join(root, "AGENTS.md"), link); err != nil {
		t.Skipf("建软链失败（跳过）: %v", err)
	}

	res, err := Load(child, Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("文件数 = %d，want 1（软链到同一份文件只算一份）", len(res.Files))
	}
}

func TestLoadMaxDepthLimitsAncestors(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "外层约定")
	writeFile(t, filepath.Join(child, "AGENTS.md"), "内层约定")

	// MaxDepth 0：只看工作目录。
	res, err := Load(child, Options{MaxDepth: 0})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 1 || !strings.Contains(res.Files[0].Content, "内层约定") {
		t.Fatalf("MaxDepth=0 应只看工作目录，实际 %d 个文件", len(res.Files))
	}

	// MaxDepth 1：工作目录 + 一层祖先。
	res, err = Load(child, Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("MaxDepth=1 应有 2 个文件，实际 %d", len(res.Files))
	}
}

func TestLoadTruncatesToByteBudget(t *testing.T) {
	dir := t.TempDir()
	// 每一行 100 字节，共 100 行（10000 字节），预算 2000 字节。
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString(strings.Repeat("字", 25) + "\n") // 每行 75 字节 + 换行
	}
	writeFile(t, filepath.Join(dir, "AGENTS.md"), sb.String())

	res, err := Load(dir, Options{MaxDepth: 0, MaxBytes: 2000})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("应标记已截断：%+v", res)
	}
	if len(res.Files) != 1 || !res.Files[0].Truncated {
		t.Fatalf("文件应标记已截断：%+v", res.Files)
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("截断必须有警告说明（不静默丢内容）")
	}
	if !strings.Contains(res.Text, "已截断") {
		t.Fatalf("正文应标注截断：%s", res.Text)
	}
	if len(res.Text) > 2000 {
		t.Fatalf("正文 %d 字节，超过预算 2000", len(res.Text))
	}
	// 裁剪必须落在 Rune 边界（不切碎多字节字符）。
	if !utf8.ValidString(res.Files[0].Content) {
		t.Fatalf("裁剪切碎了多字节字符")
	}
	// 整段文本也必须是合法 UTF-8。
	if !utf8.ValidString(res.Text) {
		t.Fatalf("正文不是合法 UTF-8")
	}
}

func TestLoadBudgetExhaustionSkipsRestAndWarns(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	// 外层文件很大：先把它装进去，预算就用光了，内层的文件装不下。
	writeFile(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("甲", 4000))
	writeFile(t, filepath.Join(child, "AGENTS.md"), "内层约定")

	res, err := Load(child, Options{MaxDepth: 1, MaxBytes: 500})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("预算用完后不该再加载文件，实际 %d 个", len(res.Files))
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "未加载") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应警告有文件未加载：%v", res.Warnings)
	}
}

func TestLoadStripsBOM(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "\ufeff带 BOM 的正文")

	res, err := Load(dir, Options{MaxDepth: 0})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("文件数 = %d，want 1", len(res.Files))
	}
	if strings.HasPrefix(res.Files[0].Content, "\ufeff") {
		t.Fatalf("BOM 未去：%q", res.Files[0].Content)
	}
}

func TestLoadRendersPathTag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	writeFile(t, path, "正文")

	res, err := Load(dir, Options{MaxDepth: 0})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "<project_instructions path=\"" + path + "\">"
	if !strings.Contains(res.Text, want) {
		t.Fatalf("正文应带来源路径标签 %q，实际：%s", want, res.Text)
	}
	if !strings.Contains(res.Text, "</project_instructions>") {
		t.Fatalf("正文缺少闭合标签：%s", res.Text)
	}
}

func TestLoadNoFilesIsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	res, err := Load(dir, Options{MaxDepth: 0})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 0 || res.Text != "" {
		t.Fatalf("没有上下文文件时应为空结果：%+v", res)
	}
}

func TestLoadMissingDirectoryIsError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope"), Options{}); err == nil {
		t.Fatalf("工作目录不存在应报错")
	}
	// 文件而不是目录：也是错误（调用方的配置错了，不是可降级的故障）。
	f := filepath.Join(t.TempDir(), "file.txt")
	writeFile(t, f, "x")
	if _, err := Load(f, Options{}); err == nil {
		t.Fatalf("工作目录不是目录应报错")
	}
}

func TestLoadWarnsOnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	writeFile(t, path, "读不动")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("改权限失败（跳过）: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	res, err := Load(dir, Options{MaxDepth: 0})
	if err != nil {
		t.Fatalf("Load 不该因单个文件读不动而失败: %v", err)
	}
	if len(res.Files) != 0 {
		t.Skipf("当前用户仍能读该文件（跳过断言）")
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("读不动要有警告：%v", res.Warnings)
	}
}

func TestLoadContinuesToNextCandidateAfterBrokenFile(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "AGENTS.override.md")
	writeFile(t, broken, "坏的 override")
	if err := os.Chmod(broken, 0o000); err != nil {
		t.Skipf("改权限失败（跳过）: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(broken, 0o644) })
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "好的约定")

	res, err := Load(dir, Options{MaxDepth: 0})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) == 1 && strings.Contains(res.Files[0].Content, "好的约定") {
		return // 读不动时退到下一个候选，符合预期
	}
	if len(res.Files) == 0 && len(res.Warnings) > 0 {
		return // 读不动且没读到下一个候选：如实警告，也不算失败
	}
	t.Fatalf("结果不符合预期：files=%+v warnings=%v", res.Files, res.Warnings)
}

// jailedReader 模拟宿主注入的「只许读某个根内」的 Reader：
// 越界的路径当作不存在（框架据此安静跳过，不越狱）。
type jailedReader struct {
	root string
}

func (r jailedReader) Stat(path string) (os.FileInfo, error) {
	if !strings.HasPrefix(path, r.root) {
		return nil, os.ErrNotExist
	}
	return os.Stat(path)
}

func (r jailedReader) ReadFile(path string) ([]byte, error) {
	if !strings.HasPrefix(path, r.root) {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(path)
}

func (r jailedReader) Canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// I/O 由宿主注入（框架不碰文件系统）：注入的 Reader 拒绝的路径，框架就该跳过。
func TestLoadUsesInjectedReader(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "根内约定")
	outsideDir := t.TempDir()
	writeFile(t, filepath.Join(outsideDir, "AGENTS.md"), "根外约定")

	// 只许读 root：根内的文件正常读到。
	res, err := Load(root, Options{MaxDepth: 0, Reader: jailedReader{root: root}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 1 || !strings.Contains(res.Files[0].Content, "根内约定") {
		t.Fatalf("应只读到根内文件：%+v", res.Files)
	}

	// 工作目录在 jail 外：Reader 读不到 → 如实报错（不静默当成「没有约定」）。
	if _, err := Load(outsideDir, Options{MaxDepth: 0, Reader: jailedReader{root: root}}); err == nil {
		t.Fatalf("工作目录读不到应报错")
	}
}

// 预算小到连一份文件都装不下：不留空标题（空正文 = 没有上下文文件）。
func TestLoadBudgetTooSmallYieldsEmptyText(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "约定")
	res, err := Load(dir, Options{MaxDepth: 0, MaxBytes: 10})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Text != "" {
		t.Fatalf("装不下时应为空正文：%q", res.Text)
	}
	if len(res.Files) != 0 {
		t.Fatalf("装不下时不该留下文件：%+v", res.Files)
	}
}

// 全局目录：MoS 自己的家是 ~/.mos（与 PI 的 ~/.pi/agent 无关）。
func TestDefaultGlobalDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("取不到 HOME（跳过）")
	}
	if got, want := DefaultGlobalDir(), filepath.Join(home, ".mos"); got != want {
		t.Fatalf("DefaultGlobalDir = %q，want %q", got, want)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("取不到 HOME（跳过）")
	}
	if got, want := ExpandHome("~/.mos"), filepath.Join(home, ".mos"); got != want {
		t.Fatalf("ExpandHome(~/.mos) = %q，want %q", got, want)
	}
	if got, want := ExpandHome("~"), home; got != want {
		t.Fatalf("ExpandHome(~) = %q，want %q", got, want)
	}
	if got, want := ExpandHome("/abs/path"), "/abs/path"; got != want {
		t.Fatalf("ExpandHome(/abs/path) = %q，want %q", got, want)
	}
}

// GlobalDirs 里写 "~/.mos" 要能展开（宿主常直接写这个）。
func TestLoadExpandsGlobalDirTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("取不到 HOME（跳过）")
	}
	global := filepath.Join(home, ".mos")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Skipf("建全局目录失败（跳过）: %v", err)
	}
	path := filepath.Join(global, "AGENTS.md")
	if err := os.WriteFile(path, []byte("全局约定"), 0o644); err != nil {
		t.Fatalf("写全局 AGENTS.md: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	dir := t.TempDir()
	res, err := Load(dir, Options{MaxDepth: 0, GlobalDirs: []string{"~/.mos"}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Files) != 1 || !strings.Contains(res.Files[0].Content, "全局约定") {
		t.Fatalf("应加载到 ~/.mos 里的全局约定：%+v", res.Files)
	}
}

// 全局约定模板：SDK 内嵌一份，宿主拿去落盘（新装的人也有约定，不是只有手写的人有）。
func TestGlobalTemplate(t *testing.T) {
	tpl := GlobalTemplate()
	if strings.TrimSpace(tpl) == "" {
		t.Fatal("全局约定模板不能为空")
	}
	for _, want := range []string{"全局约定", "先给答案", "权限"} {
		if !strings.Contains(tpl, want) {
			t.Fatalf("模板缺少 %q", want)
		}
	}
}

// EnsureGlobalFile：写一份模板；**已存在不覆盖**（用户改过的是他们的）。
func TestEnsureGlobalFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".mos")
	created, path, err := EnsureGlobalFile(dir)
	if err != nil {
		t.Fatalf("EnsureGlobalFile: %v", err)
	}
	if !created {
		t.Fatal("首次应创建")
	}
	if filepath.Base(path) != GlobalTemplateFileName {
		t.Fatalf("路径 = %q", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if string(got) != GlobalTemplate() {
		t.Fatal("写入的内容应与模板一致")
	}
	// 用户改过了：再调一次不覆盖。
	if err := os.WriteFile(path, []byte("我改过的约定"), 0o644); err != nil {
		t.Fatalf("改写: %v", err)
	}
	created, _, err = EnsureGlobalFile(dir)
	if err != nil {
		t.Fatalf("EnsureGlobalFile(2): %v", err)
	}
	if created {
		t.Fatal("已存在时不该再写")
	}
	got, _ = os.ReadFile(path)
	if string(got) != "我改过的约定" {
		t.Fatalf("用户改过的内容被覆盖了：%q", got)
	}
}
