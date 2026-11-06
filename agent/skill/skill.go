// Package skill 提供「技能」（Skill）能力：装配期发现技能目录，按需把技能正文交给模型。
//
// 站位：**渐进式披露**——元数据进上下文、正文按需加载。
// 清单（name + description）进**系统提示词**（装配期渲染一次 → 静态，保
// prompt cache；对齐 PI 的 formatSkillsForPrompt）；正文由专用 `skill`
// 工具按需取（对齐 DSH 的 tool-skill）——不走 read 工具：技能正文来自宿主
// 声明的目录，与「读工作区文件」的权限面分开。
//
// 技能集**装配期固定**：不做运行期热加载（治理规则「不做运行期插件热加载」）。
//
// **不做技能级工具收窄**：PI 的 `allowed-tools` 只存在于文档（experimental
// pre-approved list，代码无实现），DSH 连字段都没有；两家都没有「技能激活 →
// 工具面收窄」的机制，MoS 不自创。`allowed-tools` 只解析、只留痕（诊断里
// 明说「当前不生效」），执行语义归宿主（policy 名单 + 注册表可见性）。
//
// **框架不碰文件系统**（ARCHITECTURE.md §4 治理规则 5）：读文件与列目录都
// 以 Reader 接口注入，宿主选实现。技能目录列表本身就是宿主声明的授权范围，
// 框架不另行校验根集合（框架看不到宿主注入的 Reader 内部边界，越界判断
// 只能是宿主的事）。
//
// 契约（对齐 Agent Skills spec 与 PI/DSH）：
//   - 技能 = 一个目录 + 一个 SKILL.md；frontmatter 必需 name / description；
//   - name 为 kebab-case（^[a-z0-9]+(?:-[a-z0-9]+)*$，≤64 字符）；
//   - description 必需且 ≤1024 字符；
//   - disable-model-invocation: true = 模型不可见、不可加载（只能用户显式调用）；
//   - 同名技能先发现先得（按宿主给的目录优先级），碰撞出诊断。
package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// SkillFileName 是技能文件名（技能根目录下的这个文件就是技能本体）。
const SkillFileName = "SKILL.md"

// DefaultMaxBodyBytes 是单个技能正文的默认字节上限（约 16K token）。
//
// 正文进的是工具结果，那道截断网（agent.Truncator）兜得住体积；但**装不装**
// 技能是装配期的决定：正文离谱大的技能多半是放错了文件（把整本手册塞进
// SKILL.md），装进去只会挤占每一次调用的上下文——超限的技能跳过并出诊断，
// 不静默截断（截断的指令是坏指令）。
const DefaultMaxBodyBytes = 64 * 1024

// DefaultMaxCatalogBytes 是技能清单的默认字节上限（约 2K token）。
//
// 清单是**常驻**内容：它进系统提示词，每一轮都在。技能多的时候清单比
// 正文更贵——所以清单自带预算，超预算时先丢描述（保留名字），再丢条目，
// 丢了多少如实写进末尾提示（不静默丢）。
const DefaultMaxCatalogBytes = 8 * 1024

// MaxNameLength 是技能名长度上限（PI 同款：≤64 字符）。
const MaxNameLength = 64

// MaxDescriptionLength 是技能描述长度上限（PI 同款：≤1024 字符）。
const MaxDescriptionLength = 1024

// namePattern 是技能名的合法形态：kebab-case（PI 的 validateName 与
// DSH 的 isSkillName 的交集：不得首尾连字符、不得连续连字符）。
var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Metadata 是技能的 frontmatter 元数据。
type Metadata struct {
	// Name 是技能名（kebab-case，必需；缺失时回退到目录名）。
	Name string `json:"name" yaml:"name"`
	// Description 是一句话说明（必需）：模型据此判断要不要加载这个技能。
	Description string `json:"description" yaml:"description"`
	// WhenToUse 是「什么时候用」的提示（可选，进清单）。
	WhenToUse string `json:"whenToUse,omitempty" yaml:"when-to-use"`
	// AllowedTools 是「预批准工具名单」（可选）。
	//
	// **只解析、不执行**：PI 的文档口径是 experimental pre-approved list，
	// PI 本体不强制执行；DSH 连这个字段都没有。两家都没有「技能激活 → 工具面
	// 收窄」的机制，MoS 不自创——这里只留痕，执行语义归宿主（policy 名单 +
	// 注册表可见性过滤器）。装配时若该字段非空，诊断里明说「当前不生效」。
	AllowedTools []string `json:"allowedTools,omitempty" yaml:"allowed-tools"`
	// DisableModelInvocation 为 true 时模型看不到也加载不了这个技能
	// （只能由用户显式 /name 调用）。
	DisableModelInvocation bool `json:"disableModelInvocation,omitempty" yaml:"disable-model-invocation"`
	// Extra 是自由元数据（可选，原样保留；frontmatter 里的 metadata 字段）。
	//
	// 字段名不叫 Metadata：外层已经嵌了一个 Metadata 结构体，同名字段会遮蔽
	// 彼此（sk.Metadata 指的是结构体，不是这张表）。
	Extra map[string]any `json:"extra,omitempty" yaml:"metadata"`
}

// Validate 校验元数据（PI/DSH 同款约束）。返回错误说明为什么不合法。
//
// fail-closed：不合法的技能**不装**（调用方跳过并出诊断），而不是"装上再
// 让模型撞墙"。
func (m Metadata) Validate() error {
	if m.Name == "" {
		return errors.New("name 缺失")
	}
	if utf8.RuneCountInString(m.Name) > MaxNameLength {
		return fmt.Errorf("name 过长（%d 字符，上限 %d）", utf8.RuneCountInString(m.Name), MaxNameLength)
	}
	if !namePattern.MatchString(m.Name) {
		return fmt.Errorf("name %q 不是合法 kebab-case（需匹配 ^[a-z0-9]+(?:-[a-z0-9]+)*$）", m.Name)
	}
	if m.Description == "" {
		return errors.New("description 缺失")
	}
	if utf8.RuneCountInString(m.Description) > MaxDescriptionLength {
		return fmt.Errorf("description 过长（%d 字符，上限 %d）", utf8.RuneCountInString(m.Description), MaxDescriptionLength)
	}
	return nil
}

// Skill 是一个已装配的技能：元数据 + 正文位置 + 来源目录。
//
// **正文不驻留**：BodyPath 指向 SKILL.md，正文在**调用时现读**（S8.4-C，
// 抄 DSH 的「目录/正文分离」）——改了 SKILL.md 立刻生效，不重扫、不重启，
// 也不用盯文件。
type Skill struct {
	Metadata
	// Dir 是技能目录（SKILL.md 所在目录）。
	Dir string `json:"dir"`
	// BodyPath 是技能文件路径（SKILL.md）——正文在调用时从这里现读。
	BodyPath string `json:"bodyPath"`
}

// Catalog 是技能清单：持有**当前快照**，可显式重扫替换。
//
// 快照语义（S8.4-C）：一次扫描的结果是不可变的（Snapshot），Catalog 用
// 原子指针持有——正在跑的会话继续用旧快照，下一次调用看到新快照，不会出现
// 「半个目录」的中间态。
//
// 清单本身进系统提示词（见 Render），正文由 skill 工具按需取（见 Tool）。
type Catalog struct {
	dirs            []string
	reader          Reader
	maxBodyBytes    int
	maxCatalogBytes int
	snap            atomic.Pointer[Snapshot]
}

// snapshot 返回当前快照（nil = 没扫过）。
func (c *Catalog) snapshot() *Snapshot {
	if c == nil {
		return nil
	}
	return c.snap.Load()
}

// Len 返回技能总数（含仅用户可调用的）。
func (c *Catalog) Len() int {
	s := c.snapshot()
	if s == nil {
		return 0
	}
	return len(s.skills)
}

// All 返回全部技能（按装配顺序：目录优先级 → 目录内按名排序）。
func (c *Catalog) All() []Skill {
	s := c.snapshot()
	if s == nil {
		return nil
	}
	out := make([]Skill, len(s.skills))
	copy(out, s.skills)
	return out
}

// ModelVisible 返回**模型可见**的技能：disable-model-invocation 的技能
// 不在其中（模型看不到、也加载不了，只能用户显式调用）。
func (c *Catalog) ModelVisible() []Skill {
	s := c.snapshot()
	if s == nil {
		return nil
	}
	out := make([]Skill, 0, len(s.skills))
	for _, sk := range s.skills {
		if !sk.DisableModelInvocation {
			out = append(out, sk)
		}
	}
	return out
}

// ModelVisibleNames 返回模型可见的技能名（清单渲染与快照用）。
func (c *Catalog) ModelVisibleNames() []string {
	visible := c.ModelVisible()
	out := make([]string, 0, len(visible))
	for _, s := range visible {
		out = append(out, s.Name)
	}
	return out
}

// Lookup 按名字查技能（含仅用户可调用的那些——用户显式手势要用）。
func (c *Catalog) Lookup(name string) (Skill, bool) {
	s := c.snapshot()
	if s == nil {
		return Skill{}, false
	}
	for _, sk := range s.skills {
		if sk.Name == name {
			return sk, true
		}
	}
	return Skill{}, false
}

// Warnings 返回装配期的降级说明（读不动的目录、被跳过的技能、同名碰撞……）。
//
// 不静默丢内容：装不上的技能都要在这里说一句，宿主在 Describe() 快照里
// 能看见。
func (c *Catalog) Warnings() []string {
	s := c.snapshot()
	if s == nil {
		return nil
	}
	out := make([]string, len(s.warnings))
	copy(out, s.warnings)
	return out
}

// Options 是技能发现的选项。
type Options struct {
	// Dirs 是技能目录列表（按优先级从高到低）。
	//
	// 顺序即优先级：同名技能**先发现先得**（后来的被遮蔽并出诊断，PI 同款）。
	Dirs []string
	// Reader 是读文件与列目录的能力（nil = OSReader，读本地文件）。
	Reader Reader
	// MaxBodyBytes 是单个技能正文的字节上限（≤0 = DefaultMaxBodyBytes）。
	MaxBodyBytes int
	// MaxCatalogBytes 是清单渲染的字节上限（≤0 = DefaultMaxCatalogBytes）。
	MaxCatalogBytes int
}

// Load 发现并装配技能（首次扫描）。
//
// 降级而非失败：目录读不动、技能文件坏了——都**跳过这个技能**并记进
// Warnings，其余技能照常装（对齐 agent/contextfile 的降级纪律：不静默丢
// 内容）。唯一的硬失败是宿主给了一个空的 Dirs 列表之外的
// 「无法继续」的情况——目前没有：装配期不因为技能坏掉而拒绝启动。
//
// **正文不读进内存**：目录扫描只取 frontmatter 元数据与正文路径，正文在
// 调用时现读（S8.4-C）。
func Load(opts Options) *Catalog {
	c := &Catalog{
		dirs:         append([]string(nil), opts.Dirs...),
		reader:       opts.Reader,
		maxBodyBytes: opts.MaxBodyBytes,
	}
	if c.reader == nil {
		c.reader = OSReader{}
	}
	if c.maxBodyBytes <= 0 {
		c.maxBodyBytes = DefaultMaxBodyBytes
	}
	c.maxCatalogBytes = opts.MaxCatalogBytes
	if c.maxCatalogBytes <= 0 {
		c.maxCatalogBytes = DefaultMaxCatalogBytes
	}
	c.snap.Store(c.scan())
	return c
}

// loadDir 装配一个目录下的技能。
//
// 规则（PI 的 loadSkillsFromDirInternal 同款，MoS 只吃 spec 形态）：
//   - 目录自己含 SKILL.md → 它就是技能根（不再下钻，PI 同款）；
//   - 否则它的每个直接子目录（含 SKILL.md）是一个技能；
//   - 裸 .md 文件不算技能（PI 支持把 .md 当技能，MoS v1 不吃这一形态：
//     裸文件没有「技能目录」，资源基准无从谈起——宿主想用就转成目录）。
func loadDir(dir string, reader Reader, warnings *[]string) []Skill {
	if raw, ok := readSkillFile(dir, reader, warnings); ok {
		if s, ok := buildSkill(dir, raw, warnings); ok {
			return []Skill{s}
		}
		return nil
	}
	entries, err := reader.ReadDir(dir)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("技能目录 %q 读不动: %v", dir, err))
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir || strings.HasPrefix(e.Name, ".") {
			continue
		}
		names = append(names, e.Name)
	}
	sort.Strings(names) // 确定性输出：同一目录的技能顺序与文件系统返回顺序无关
	out := make([]Skill, 0, len(names))
	for _, n := range names {
		sub := filepath.Join(dir, n)
		raw, ok := readSkillFile(sub, reader, warnings)
		if !ok {
			continue
		}
		if s, ok := buildSkill(sub, raw, warnings); ok {
			out = append(out, s)
		}
	}
	return out
}

// readSkillFile 读技能目录下的 SKILL.md。
//
// 「没有这个文件」是常态（目录不是技能根）→ 不出声；「有但读不动」是异常
// → 出诊断。
func readSkillFile(dir string, reader Reader, warnings *[]string) ([]byte, bool) {
	path := filepath.Join(dir, SkillFileName)
	raw, err := reader.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false
		}
		*warnings = append(*warnings, fmt.Sprintf("技能文件 %q 读不动: %v", path, err))
		return nil, false
	}
	return raw, true
}

// buildSkill 解析并校验一个技能文件。合法才返回技能。
//
// **只取 frontmatter**：正文不驻留（调用时现读）；正文体积在调用时校验，
// 不在装配期判死（超限的技能到调用时报错，而不是从清单里消失）。
func buildSkill(dir string, raw []byte, warnings *[]string) (Skill, bool) {
	front, body, ok := splitFrontmatter(raw)
	if !ok {
		*warnings = append(*warnings, fmt.Sprintf("技能 %q 缺少 frontmatter（文件须以 --- 开头）", filepath.Join(dir, SkillFileName)))
		return Skill{}, false
	}
	meta, err := parseMetadata(front)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("技能 %q 的 frontmatter 解析失败: %v", filepath.Join(dir, SkillFileName), err))
		return Skill{}, false
	}
	if meta.Name == "" {
		meta.Name = filepath.Base(dir) // PI 同款：name 缺失回退父目录名
	}
	if err := meta.Validate(); err != nil {
		*warnings = append(*warnings, fmt.Sprintf("技能 %q 不合法: %v", filepath.Join(dir, SkillFileName), err))
		return Skill{}, false
	}
	if len(meta.AllowedTools) > 0 {
		*warnings = append(*warnings, fmt.Sprintf(
			"技能 %q 声明了 allowed-tools，但该字段当前不生效（PI 有字段无实现、DSH 无此字段；技能级工具收窄不自创）——请用宿主策略实现", meta.Name))
	}
	_ = body // 正文不驻留：调用时现读。
	return Skill{Metadata: meta, Dir: dir, BodyPath: filepath.Join(dir, SkillFileName)}, true
}

// splitFrontmatter 拆出 frontmatter 与正文。
//
// 只认**文件开头**的 frontmatter（首行必须是 ---）：正文里出现的 ---
// 不是分隔符（PI 同款：开头解析一次）。
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

// parseMetadata 解析 frontmatter。
//
// 走「先解成 map 再挑字段」的路子，而不是直接解进结构体：frontmatter 是
// **宿主写的文件**，字段类型可能不认（metadata 写成字符串、布尔写成
// "true"）。直接解结构体遇到一个怪字段就整份报废；解成 map 再挑，坏的
// 单个字段被忽略，其余照常能用。
func parseMetadata(front string) (Metadata, error) {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(front), &raw); err != nil {
		return Metadata{}, fmt.Errorf("不是合法 YAML: %w", err)
	}
	m := Metadata{
		Name:                   asString(raw["name"]),
		Description:            asString(raw["description"]),
		WhenToUse:              asString(raw["when-to-use"]),
		AllowedTools:           asStringList(raw["allowed-tools"]),
		DisableModelInvocation: asBool(raw["disable-model-invocation"]),
	}
	if md, ok := raw["metadata"].(map[string]any); ok {
		m.Extra = md
	}
	return m, nil
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

// asBool 取布尔字段（也接受 "true"/"false" 字符串——YAML 里手写字符串很常见）。
func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := parseBoolText(t)
		return err == nil && b
	}
	return false
}

// parseBoolText 解析 "true"/"false"（大小写不敏感）。
func parseBoolText(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes":
		return true, nil
	case "false", "no":
		return false, nil
	}
	return false, fmt.Errorf("%q 不是布尔值", s)
}

// Reader 是技能目录的 I/O 能力（框架不碰文件系统，宿主注入）。
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
