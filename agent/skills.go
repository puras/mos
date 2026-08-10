package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// SkillFile 是技能声明文件名（对齐 Agent Skills 规范：SKILL.md）。
const SkillFile = "SKILL.md"

// Skill 是扫描到的一个技能：SKILL.md frontmatter 声明的 name 与 description（触发条件）。
// 技能正文与 references/ 目录由模型在触发时经 read_file 按需读取——框架只注入
// 懒加载清单，正文不占常驻上下文（对齐 Claude Code / Agent Skills 的懒加载语义）。
type Skill struct {
	// Name 是技能名（frontmatter name）。
	Name string
	// Description 是技能的触发条件与能力描述（frontmatter description）。
	Description string
	// Dir 是 SKILL.md 所在目录。
	Dir string
}

// LoadSkills 扫描 dir 中的技能：
//   - dir 直接包含的 SKILL.md；
//   - dir 下每个一级子目录中的 SKILL.md。
//
// 未解析出 name（无 frontmatter 或缺 name 键）的技能被跳过。
// 目录不存在时返回空列表（非错误）。
func LoadSkills(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Skill
	if s, ok := parseSkillFile(filepath.Join(dir, SkillFile)); ok {
		out = append(out, s)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if s, ok := parseSkillFile(filepath.Join(dir, e.Name(), SkillFile)); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// parseSkillFile 解析单个 SKILL.md，返回技能（缺少 frontmatter 的 name 时不识别）。
func parseSkillFile(path string) (Skill, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, false
	}
	meta, ok := parseFrontmatter(b)
	if !ok {
		return Skill{}, false
	}
	s := Skill{Dir: filepath.Dir(path)}
	s.Name = strings.TrimSpace(meta["name"])
	if s.Name == "" {
		return Skill{}, false
	}
	s.Description = strings.TrimSpace(meta["description"])
	return s, true
}

// parseFrontmatter 解析 SKILL.md 顶部的 frontmatter 块（首个 `---` 到第二个 `---` 之间），
// 仅提取 name 与 description 键。非键值对行（注释等）被忽略。
func parseFrontmatter(b []byte) (map[string]string, bool) {
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return nil, false
	}
	meta := make(map[string]string)
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return meta, true
		}
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k != "name" && k != "description" {
			continue
		}
		meta[k] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return nil, false
}

// renderSkills 将技能清单渲染为懒加载目录文本（注入系统提示词的技能槽位）。
// 提示模型按需触发，正文经 read_file 读取。
func renderSkills(skills []Skill) string {
	var b strings.Builder
	b.WriteString("以下技能按需触发（技能正文与引用文件经 read_file 懒加载读取）：\n")
	for _, s := range skills {
		b.WriteString("- ")
		b.WriteString(s.Name)
		if d := strings.TrimSpace(s.Description); d != "" {
			b.WriteString(": ")
			b.WriteString(d)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
