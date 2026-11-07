// Package subagent 提供「子代理」（SubAgent）能力：装配期发现子代理目录，
// 把清单渲染进系统提示词；模型点名调用某个子代理，由宿主注入的 Runner 执行。
//
// 站位：**子代理 = 一个普通工具**（agents-as-tools，PRD FR-D3）。主 Agent
// 用 function calling 点名调用 `subagent__<name>`，子代理在自己的会话里用
// **收窄过的工具面**干活，只回一段**固定结构**的回执
// （`{status, summary, findings[]}`）。
//
// 本包只管**声明与接缝**，不造 Agent：
//
//   - Definition：frontmatter 声明（name / description / model / tools +
//     instructions 正文）；目录形态与 Skills 同构
//     （`<subagent-dir>/<name>/AGENT.md`）；
//   - Catalog：目录发现（装配期固定，v1 不做运行期热加载）；
//   - Render：清单渲染进**静态**系统提示词（装配期渲染一次 → 保 prompt cache）；
//   - Runner：**宿主注入**的执行体接口（治理规则 5：框架不绑宿主形态；也
//     避免根包与本包成环）；
//   - Result：固定结构回执（status / summary / findings）。
//
// 声明式子代理目录是 **MoS 自创**（ARCHITECTURE.md §3.18 ⑤）：PI 没有子代理
// 委派层（只有可复用的 agent loop 与 codemode 替代路径），DSH 的子代理是
// **运行期创建的会话**（描述写在 prompt 里），没有「角色目录」这一层。
// 风险是多一套目录格式 → 缓解：与 Skills 同构（同一个 `Reader` 注入形态）。
//
// **框架不碰文件系统**（治理规则 5）：读文件与列目录都走注入的 Reader。
// 子代理目录列表本身就是宿主声明的授权范围，框架不另行校验根集合
// （框架看不到 Reader 内部边界，越界判断只能是宿主的事）。
package subagent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// FileName 是子代理定义文件名（子代理目录下的这个文件就是子代理本体）。
	FileName = "AGENT.md"

	// ToolPrefix 是子代理工具名前缀（工具名 = ToolPrefix + 子代理名）。
	//
	// 命名对齐 MCP 的 `mcp__<server>__<tool>` 命名空间惯例（2026-11-06 拍板）。
	ToolPrefix = "subagent__"
)

const (
	// MaxNameLength 是子代理名长度上限（同 Skills 口径：≤64 字符）。
	MaxNameLength = 64

	// MaxDescriptionLength 是描述长度上限（同 Skills 口径：≤1024 字符）。
	MaxDescriptionLength = 1024
)

// DefaultMaxCatalogBytes 是子代理清单的默认字节上限（约 2K token）。
//
// 清单是**常驻**内容（进系统提示词，每一轮都在），所以自带预算。降级策略
// 照抄 Skills：先丢描述（保留名字），再丢条目，丢了多少如实写进末尾提示
// （不静默丢——模型知道还有子代理存在，只是清单里没列）。
const DefaultMaxCatalogBytes = 8 * 1024

// namePattern 是子代理名的合法形态：kebab-case（同 Skills 口径：
// 不得首尾连字符、不得连续连字符）。
var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Definition 是一个子代理的声明（frontmatter + 正文位置）。
//
// **正文不驻留**：InstructionsPath 指向 AGENT.md，指令正文在**调用时现读**
// （同 Skills 的 S8.4-C 口径：改了 AGENT.md 立刻生效，不必重扫）。
type Definition struct {
	// Name 是子代理名（kebab-case，必需；缺失时回退到目录名）。
	Name string `json:"name"`
	// Description 是一句话说明（必需）：模型据此判断该不该派这个子代理。
	Description string `json:"description"`
	// Model 是**模型覆盖**（可选）：声明了但宿主目录里没有 → 装配期报错
	// （「声明了却没装 = 假能力」，2026-11-06 拍板），不静默降级到父模型。
	Model string `json:"model,omitempty"`
	// Tools 是**声明白名单**（可选）：子代理实际能用的工具 =
	// 声明白名单 ∩ 父可见工具集 ∩ 宿主 policy（deny 优先）。
	//
	// 空 = 不收窄（继承父的可见集）。**声明不能放大宿主策略**：作者写了
	// 不等于策略（DSH 同款：toolFilter + 父策略向下委派）。
	Tools []string `json:"tools,omitempty"`
	// Dir 是子代理目录（AGENT.md 所在目录）。
	Dir string `json:"dir"`
	// InstructionsPath 是定义文件路径（AGENT.md）——指令正文在调用时从这里现读。
	InstructionsPath string `json:"instructionsPath"`
	// HasInstructions 是「指令正文非空」（扫描时判定）。
	//
	// 只用于拆分判据（PRD FR-D3：instructions / tools / model 真正改变才拆）：
	// 正文非空 = 这个子代理有独立人设。正文改了要重扫才会更新这个标记
	// （它是**装配期诊断**的输入，不是运行时事实）。
	HasInstructions bool `json:"hasInstructions"`
}

// Validate 校验声明。返回错误说明为什么不合法。
//
// fail-closed：不合法的子代理**不装**（调用方跳过并出诊断），而不是
// 「装上再让模型撞墙」。
func (d Definition) Validate() error {
	if d.Name == "" {
		return errors.New("name 缺失")
	}
	if utf8.RuneCountInString(d.Name) > MaxNameLength {
		return fmt.Errorf("name 过长（%d 字符，上限 %d）", utf8.RuneCountInString(d.Name), MaxNameLength)
	}
	if !namePattern.MatchString(d.Name) {
		return fmt.Errorf("name %q 不是合法 kebab-case（需匹配 ^[a-z0-9]+(?:-[a-z0-9]+)*$）", d.Name)
	}
	if d.Description == "" {
		return errors.New("description 缺失")
	}
	if utf8.RuneCountInString(d.Description) > MaxDescriptionLength {
		return fmt.Errorf("description 过长（%d 字符，上限 %d）", utf8.RuneCountInString(d.Description), MaxDescriptionLength)
	}
	return nil
}

// ToolName 返回子代理的工具名（`subagent__<name>`）。
func ToolName(name string) string { return ToolPrefix + name }

// Catalog 是子代理清单：装配期发现的结果（不可变）。
//
// **装配期固定**：v1 不做运行期热加载（同 Skills 在 S8.1 时的纪律；Skills
// 后来加了显式 reload，SubAgent v1 不加——子代理集比技能集小得多）。
type Catalog struct {
	defs            []Definition
	warnings        []string
	reader          Reader
	maxCatalogBytes int
	modelProbe      ModelProbe
}

// Options 是子代理发现的选项。
type Options struct {
	// Dirs 是子代理目录列表（按优先级从高到低）。
	//
	// 顺序即优先级：同名子代理**先发现先得**（后来的被遮蔽并出诊断，
	// 同 Skills 口径）。目录列表本身 = **宿主声明的授权边界**。
	Dirs []string
	// Reader 是读文件与列目录的能力（nil = OSReader，读本地文件）。
	Reader Reader
	// MaxCatalogBytes 是清单渲染的字节上限（≤0 = DefaultMaxCatalogBytes）。
	MaxCatalogBytes int
	// ModelProbe 探测宿主目录里有没有某个模型（可选；nil = 不校验模型覆盖）。
	//
	// 装配层接宿主自己的模型目录：声明了却不存在的模型 = 假能力，
	// 装配期报错（不静默降级到父模型）。
	ModelProbe ModelProbe
}

// Load 发现并装配子代理（一次性扫描）。
//
// 降级而非失败：目录读不动、定义文件坏了——都**跳过这个子代理**并记进
// Warnings，其余照常装（对齐 Skills / contextfile 的降级纪律：不静默丢
// 内容）。装配期不因为某个子代理坏掉而拒绝启动。
func Load(opts Options) *Catalog {
	reader := opts.Reader
	if reader == nil {
		reader = OSReader{}
	}
	budget := opts.MaxCatalogBytes
	if budget <= 0 {
		budget = DefaultMaxCatalogBytes
	}
	c := &Catalog{
		reader:          reader,
		maxCatalogBytes: budget,
		modelProbe:      opts.ModelProbe,
	}
	seen := make(map[string]Definition)
	for _, dir := range opts.Dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		for _, d := range loadDir(dir, reader, &c.warnings) {
			if prev, dup := seen[d.Name]; dup {
				c.warnings = append(c.warnings, fmt.Sprintf(
					"子代理 %q 重名：保留 %s，跳过 %s（先到先得）", d.Name, prev.Dir, d.Dir))
				continue
			}
			seen[d.Name] = d
			c.defs = append(c.defs, d)
		}
	}
	return c
}

// Len 返回子代理总数。
func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.defs)
}

// All 返回全部子代理（按装配顺序：目录优先级 → 目录内按名排序）。
func (c *Catalog) All() []Definition {
	if c == nil {
		return nil
	}
	out := make([]Definition, len(c.defs))
	copy(out, c.defs)
	return out
}

// Names 返回全部子代理名（清单渲染与快照用）。
func (c *Catalog) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.defs))
	for _, d := range c.defs {
		out = append(out, d.Name)
	}
	return out
}

// Lookup 按名字查子代理。
func (c *Catalog) Lookup(name string) (Definition, bool) {
	if c == nil {
		return Definition{}, false
	}
	for _, d := range c.defs {
		if d.Name == name {
			return d, true
		}
	}
	return Definition{}, false
}

// Warnings 返回装配期的降级说明（读不动的目录、被跳过的定义、同名碰撞……）。
//
// 不静默丢内容：装不上的子代理都要在这里说一句，宿主在 Describe() 快照里
// 能看见。
func (c *Catalog) Warnings() []string {
	if c == nil {
		return nil
	}
	out := make([]string, len(c.warnings))
	copy(out, c.warnings)
	return out
}

// ReadInstructions 读子代理的指令正文（现读）。
//
// 读不动就报错（fail-closed）：指令正文是子代理的人设，缺了它就跑成
// 「没有角色的子代理」——比不跑更坏。
func (c *Catalog) ReadInstructions(def Definition) (string, error) {
	if c == nil || c.reader == nil {
		return "", errors.New("没有可用的 Reader")
	}
	raw, err := c.reader.ReadFile(def.InstructionsPath)
	if err != nil {
		return "", fmt.Errorf("读子代理 %q 的定义文件 %q 失败: %w", def.Name, def.InstructionsPath, err)
	}
	_, body, ok := splitFrontmatter(raw)
	if !ok {
		// 装配期已校验过 frontmatter 存在；这里仍兜一层（文件被改过）。
		return "", fmt.Errorf("子代理 %q 的定义文件缺少 frontmatter", def.Name)
	}
	return strings.TrimSpace(body), nil
}

// BuildSpec 造一次子代理调用的输入：读指令正文（现读）+ 带上声明的白名单
// 与模型覆盖。
//
// 任务与上下文由父模型给（工具参数）；工具面与模型覆盖来自声明——**实际**
// 工具面由执行体按「白名单 ∩ 父可见集 ∩ 宿主 policy」收窄（见 ResolveTools）。
func (c *Catalog) BuildSpec(def Definition, task, extraContext string) (Spec, error) {
	instructions, err := c.ReadInstructions(def)
	if err != nil {
		return Spec{}, err
	}
	return Spec{
		Name:         def.Name,
		Task:         task,
		Context:      extraContext,
		Instructions: instructions,
		Tools:        append([]string(nil), def.Tools...),
		Model:        def.Model,
	}, nil
}

// loadDir 装配一个目录下的子代理。
//
// 规则（与 Skills 同构）：
//   - 目录自己含 AGENT.md → 它就是子代理根（不再下钻）；
//   - 否则它的每个直接子目录（含 AGENT.md）是一个子代理；
//   - 裸 .md 文件不算子代理（没有「子代理目录」，角色基准无从谈起）。
func loadDir(dir string, reader Reader, warnings *[]string) []Definition {
	if raw, ok := readDefinitionFile(dir, reader, warnings); ok {
		if d, ok := buildDefinition(dir, raw, warnings); ok {
			return []Definition{d}
		}
		return nil
	}
	entries, err := reader.ReadDir(dir)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("子代理目录 %q 读不动: %v", dir, err))
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir || strings.HasPrefix(e.Name, ".") {
			continue
		}
		names = append(names, e.Name)
	}
	sort.Strings(names) // 确定性输出：同一目录的顺序与文件系统返回顺序无关
	out := make([]Definition, 0, len(names))
	for _, n := range names {
		sub := filepath.Join(dir, n)
		raw, ok := readDefinitionFile(sub, reader, warnings)
		if !ok {
			continue
		}
		if d, ok := buildDefinition(sub, raw, warnings); ok {
			out = append(out, d)
		}
	}
	return out
}

// readDefinitionFile 读子代理目录下的 AGENT.md。
//
// 「没有这个文件」是常态（目录不是子代理根）→ 不出声；「有但读不动」是异常
// → 出诊断。
func readDefinitionFile(dir string, reader Reader, warnings *[]string) ([]byte, bool) {
	path := filepath.Join(dir, FileName)
	raw, err := reader.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false
		}
		*warnings = append(*warnings, fmt.Sprintf("子代理定义文件 %q 读不动: %v", path, err))
		return nil, false
	}
	return raw, true
}

// buildDefinition 解析并校验一个定义文件。合法才返回定义。
//
// **只取 frontmatter**：指令正文不驻留（调用时现读）；正文体积在调用时
// 处理，不在装配期判死（超限的子代理到调用时报错，而不是从清单里消失）。
func buildDefinition(dir string, raw []byte, warnings *[]string) (Definition, bool) {
	path := filepath.Join(dir, FileName)
	front, body, ok := splitFrontmatter(raw)
	if !ok {
		*warnings = append(*warnings, fmt.Sprintf("子代理 %q 缺少 frontmatter（文件须以 --- 开头）", path))
		return Definition{}, false
	}
	meta, err := parseDefinition(front)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("子代理 %q 的 frontmatter 解析失败: %v", path, err))
		return Definition{}, false
	}
	if meta.Name == "" {
		meta.Name = filepath.Base(dir) // 同 Skills 口径：name 缺失回退父目录名
	}
	d := Definition{
		Name:             meta.Name,
		Description:      meta.Description,
		Model:            meta.Model,
		Tools:            meta.Tools,
		Dir:              dir,
		InstructionsPath: path,
		HasInstructions:  strings.TrimSpace(body) != "",
	}
	if err := d.Validate(); err != nil {
		*warnings = append(*warnings, fmt.Sprintf("子代理 %q 不合法: %v", path, err))
		return Definition{}, false
	}
	return d, true
}

// splitFrontmatter 拆出 frontmatter 与正文。
//
// 只认**文件开头**的 frontmatter（首行必须是 ---）：正文里出现的 ---
// 不是分隔符（同 Skills 口径：开头解析一次）。
func splitFrontmatter(raw []byte) (front string, body string, ok bool) {
	text := strings.TrimPrefix(string(raw), "\ufeff")
	if !strings.HasPrefix(text, "---") {
		return "", text, false
	}
	rest := strings.TrimPrefix(text, "---")
	rest = strings.TrimPrefix(rest, "\r")
	rest = strings.TrimPrefix(rest, "\n")
	lines := strings.Split(rest, "\n")
	for i, ln := range lines {
		if strings.TrimRight(ln, " \t\r") == "---" {
			return strings.Join(lines[:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", text, false
}

// parseDefinition 解析 frontmatter。
//
// 走「先解成 map 再挑字段」的路子，而不是直接解进结构体：frontmatter 是
// **宿主写的文件**，字段类型可能不认（tools 写成字符串、model 写成数字）。
// 直接解结构体遇到一个怪字段就整份报废；解成 map 再挑，坏的单个字段被
// 忽略，其余照常能用。
//
// 解析辅助函数与 agent/skill 各写一份（不抽公共包）：两个包的 frontmatter
// 字段集不同，且新建公共包要 PRD 有 FR 指名（治理规则 1）。
func parseDefinition(front string) (Definition, error) {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(front), &raw); err != nil {
		return Definition{}, fmt.Errorf("不是合法 YAML: %w", err)
	}
	return Definition{
		Name:        asString(raw["name"]),
		Description: asString(raw["description"]),
		Model:       asString(raw["model"]),
		Tools:       asStringList(raw["tools"]),
	}, nil
}

// asString 取字符串字段（非字符串 → 空串）。
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// asStringList 取字符串列表字段：接受列表，也接受单个字符串（当一项）。
func asStringList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
	}
	return nil
}

// Reader 是子代理目录的 I/O 能力（框架不碰文件系统，宿主注入）。
//
// 两个动作够用：读文件、列目录。要 jail 语义（只许读宿主声明的目录内）
// 就注入自己的 Reader——框架不替宿主决定能读哪儿。
type Reader interface {
	// ReadFile 读文件内容。
	ReadFile(path string) ([]byte, error)
	// ReadDir 列目录项。
	ReadDir(path string) ([]Entry, error)
}

// Entry 是目录项。
type Entry struct {
	// Name 是项名（不含路径）。
	Name string
	// IsDir 为 true 表示是子目录。
	IsDir bool
}

// OSReader 是直接读本地文件的 Reader（宿主不注入时的默认实现）。
type OSReader struct{}

// ReadFile 实现 Reader。
func (OSReader) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// ReadDir 实现 Reader。
func (OSReader) ReadDir(path string) ([]Entry, error) {
	des, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		out = append(out, Entry{Name: de.Name(), IsDir: de.IsDir()})
	}
	return out, nil
}
