package agent

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillFile 是技能声明文件名（对齐 Agent Skills 规范：SKILL.md）。
const SkillFile = "SKILL.md"

// Skill 是扫描到的一个技能：SKILL.md frontmatter 声明的元数据。
// 技能正文与 references/ 目录由模型在触发时经 read_file 按需读取——框架只注入
// 懒加载清单，正文不占常驻上下文（对齐 Claude Code / Agent Skills 的懒加载语义）。
type Skill struct {
	// Name 是技能名（frontmatter name）。
	Name string
	// Description 是技能的触发条件与能力描述（frontmatter description）。
	Description string
	// SchemaVersion 是 frontmatter schema 版本（约定为字符串，向后兼容）。
	SchemaVersion string
	// WhenToUse 是 frontmatter 中模型触发条件的额外描述。
	WhenToUse string
	// AllowedTools 是技能允许使用的工具白名单（frontmatter allowed-tools）。
	AllowedTools []string
	// References 是技能引用文件路径列表（frontmatter references）。
	References []string
	// Paths 是技能生效路径 glob 列表（frontmatter paths）。
	Paths []string
	// Source 是技能来源（frontmatter source，如 local/repository）。
	Source string
	// UserInvocable 表示是否允许用户直接调用（frontmatter user-invocable）。
	UserInvocable bool

	// Dir 是 SKILL.md 所在目录。
	Dir string
	// PathMatch 是当前 working dir 是否命中 paths glob（运行时计算）。
	PathMatch bool
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
	// SKILL.md = `---<yaml>---\n<markdown body>`
	// 用 yaml.Unmarshal 解析首段 frontmatter（遇到非键值对行停止）
	meta := struct {
		Name          string   `yaml:"name"`
		Description   string   `yaml:"description"`
		SchemaVersion string   `yaml:"schema_version"`
		WhenToUse     string   `yaml:"when_to_use"`
		AllowedTools  []string `yaml:"allowed-tools"`
		References    []string `yaml:"references"`
		Paths         []string `yaml:"paths"`
		Source        string   `yaml:"source"`
		UserInvocable bool     `yaml:"user-invocable"`
	}{}
	if err := yaml.Unmarshal(extractFrontmatter(b), &meta); err != nil {
		return Skill{}, false
	}
	s := Skill{
		Name:          strings.TrimSpace(meta.Name),
		Description:   strings.TrimSpace(meta.Description),
		SchemaVersion: strings.TrimSpace(meta.SchemaVersion),
		WhenToUse:     strings.TrimSpace(meta.WhenToUse),
		AllowedTools:  meta.AllowedTools,
		References:    meta.References,
		Paths:         meta.Paths,
		Source:        strings.TrimSpace(meta.Source),
		UserInvocable: meta.UserInvocable,
		Dir:           filepath.Dir(path),
	}
	if s.Name == "" {
		return Skill{}, false
	}
	return s, true
}

// extractFrontmatter 提取 `---` 之间的 YAML 块（含末尾 ---）。
func extractFrontmatter(b []byte) []byte {
	s := string(b)
	if !strings.HasPrefix(strings.TrimSpace(s), "---") {
		return nil
	}
	rest := strings.TrimPrefix(strings.TrimSpace(s), "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil
	}
	return []byte(rest[:end])
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
