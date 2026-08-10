package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectDocFile 是项目指令文件名（对齐 Claude Code 的 CLAUDE.md 约定）。
const ProjectDocFile = "CLAUDE.md"

// ProjectDoc 是一份项目指令文档（来自一次向上递归扫描到的 CLAUDE.md）。
type ProjectDoc struct {
	// Path 是文档的绝对路径（渲染时用于标注来源）。
	Path string
	// Content 是文档原文。
	Content string
}

// LoadProjectDocs 从 dir 开始向上递归查找 CLAUDE.md，
// 返回按"顶层在前、最近在后"排序的文档列表——父目录指令先声明，子目录就近覆盖。
// 目录不存在或没有 CLAUDE.md 时返回空列表（非错误）。
func LoadProjectDocs(dir string) ([]ProjectDoc, error) {
	var docs []ProjectDoc
	cur := dir
	for cur != "" {
		path := filepath.Join(cur, ProjectDocFile)
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("agent: read project doc %s: %w", path, err)
			}
			docs = append([]ProjectDoc{{Path: path, Content: string(b)}}, docs...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return docs, nil
}

// renderProjectDocs 将多份项目指令渲染为分段文本：每份标注来源路径，按序拼接。
func renderProjectDocs(docs []ProjectDoc) string {
	var b strings.Builder
	for _, d := range docs {
		content := strings.TrimSpace(d.Content)
		if content == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("## ")
		b.WriteString(d.Path)
		b.WriteString("\n")
		b.WriteString(content)
	}
	return b.String()
}
