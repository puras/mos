package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProjectDocsUpwardRecursion(t *testing.T) {
	root := t.TempDir()
	// root/CLAUDE.md 与 root/a/b/CLAUDE.md 两级
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "顶层指令")
	writeFile(t, filepath.Join(root, "a", "b", "CLAUDE.md"), "子目录指令")

	docs, err := LoadProjectDocs(filepath.Join(root, "a", "b"))
	if err != nil {
		t.Fatalf("LoadProjectDocs: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs = %d, want 2", len(docs))
	}
	// 顶层在前、最近在后（子目录就近覆盖）
	if !strings.Contains(docs[0].Content, "顶层指令") {
		t.Fatalf("docs[0] = %+v, want top-level first", docs[0])
	}
	if !strings.Contains(docs[1].Content, "子目录指令") {
		t.Fatalf("docs[1] = %+v, want nearest last", docs[1])
	}
}

func TestLoadProjectDocsMissingDir(t *testing.T) {
	docs, err := LoadProjectDocs(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("docs = %+v, want empty", docs)
	}
}

func TestRenderProjectDocs(t *testing.T) {
	docs := []ProjectDoc{
		{Path: "/repo/CLAUDE.md", Content: "顶层"},
		{Path: "/repo/sub/CLAUDE.md", Content: "子层"},
	}
	got := renderProjectDocs(docs)
	if !strings.Contains(got, "/repo/CLAUDE.md") || !strings.Contains(got, "顶层") {
		t.Fatalf("render = %q", got)
	}
	if !strings.Contains(got, "/repo/sub/CLAUDE.md") || !strings.Contains(got, "子层") {
		t.Fatalf("render = %q", got)
	}
}

// 组装器集成：配置 ProjectDocDir 后，项目指令槽位注入 CLAUDE.md 内容。
func TestSystemAssemblerProjectDoc(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "本项目禁止使用 rm")

	asm := NewDefaultSystemAssembler(SystemAssemblerConfig{
		Constraints:   "你是助手",
		ProjectDocDir: root,
	})
	sections, err := asm.Assemble(t.Context(), NewSession("s1"), nil)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	text := RenderSystem(sections)
	if !strings.Contains(text, "【项目指令】") || !strings.Contains(text, "本项目禁止使用 rm") {
		t.Fatalf("system = %q, want project doc slot", text)
	}
	if strings.Index(text, "【系统约束】") > strings.Index(text, "【项目指令】") {
		t.Fatalf("project doc should follow constraints: %q", text)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
