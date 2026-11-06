package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Snapshot 是一次目录扫描的**不可变**结果。
//
// 不可变是刻意的：正在跑的会话持有旧快照继续用，新扫描的结果整体替换
// 指针——不会出现「半个目录」的中间态（有的技能是新版本、有的是旧版本）。
type Snapshot struct {
	skills   []Skill
	warnings []string
	// digest 是目录摘要（技能名 + 元数据的稳定指纹）。
	//
	// **正文不进摘要**：正文编辑不改变目录（DSH 同款：正文每次加载都重读，
	// 所以正文编辑不需要 digest / 修订号 / 缓存失效）→ 只改正文不需要
	// reload，也不需要通知模型。
	digest string
}

// ReloadResult 是重扫的结果（宿主拿它决定要不要给模型补一条消息）。
type ReloadResult struct {
	// Changed 为 true 表示目录摘要变了（快照已替换）。
	Changed bool
	// Added / Removed / Updated 是变化明细（技能名）。
	Added   []string
	Removed []string
	Updated []string
	// Notice 是给模型的 user 侧短消息（Changed 时非空）。
	//
	// 语义是 DSH 的「**替换目录**」：内容是**当前完整清单**，不是增量
	// 描述——模型据此把旧清单换成新的（静态系统提示词不动，保 prompt cache）。
	// 宿主把它追加到会话里即可。
	Notice string
}

// scan 扫一遍技能目录，产出一个新快照。
//
// 全量扫描：不复用上次的任何东西——目录可能增删改名，增量算不出「谁没了」。
func (c *Catalog) scan() *Snapshot {
	var skills []Skill
	var warnings []string
	seen := make(map[string]Skill)
	for _, dir := range c.dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		for _, s := range loadDir(dir, c.reader, &warnings) {
			if prev, dup := seen[s.Name]; dup {
				warnings = append(warnings, fmt.Sprintf(
					"技能 %q 重名：保留 %s，跳过 %s（先到先得）", s.Name, prev.Dir, s.Dir))
				continue
			}
			seen[s.Name] = s
			skills = append(skills, s)
		}
	}
	return &Snapshot{skills: skills, warnings: warnings, digest: digestOf(skills)}
}

// Reload 重扫技能目录（显式、幂等、全量）。
//
// 幂等：目录摘要没变就**不换快照**（Changed=false、Notice 空）——宿主可以
// 无脑调（比如每条用户消息前调一次）。
//
// 全量：目录可能增删改名，增量扫不出「谁没了」。
//
// 返回值给宿主：Changed 为 true 时，Notice 是给模型的 user 侧短消息
// （DSH 的「替换目录」语义），宿主追加到会话即可。
//
// 注意：**静态系统提示词不重渲染**（保 prompt cache）——清单变化只走
// user 侧消息。
func (c *Catalog) Reload() ReloadResult {
	if c == nil {
		return ReloadResult{}
	}
	next := c.scan()
	cur := c.snapshot()
	skillsChanged := cur == nil || cur.digest != next.digest
	warnsChanged := cur == nil || !sameStrings(cur.warnings, next.warnings)
	if !skillsChanged && !warnsChanged {
		return ReloadResult{Changed: false}
	}
	// 快照总是换（诊断要新鲜）；但**只有有效技能集合变了**才通知模型
	//（新增一个坏技能不打扰模型——它压根不在清单里）。
	c.snap.Store(next)
	if !skillsChanged {
		return ReloadResult{Changed: false}
	}
	return c.diff(cur, next)
}

// sameStrings 比对两个字符串切片（顺序敏感）。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diff 比对新旧快照，给出变化明细与通知文案。
func (c *Catalog) diff(old, next *Snapshot) ReloadResult {
	res := ReloadResult{Changed: true}
	oldByName := map[string]Skill{}
	if old != nil {
		for _, s := range old.skills {
			oldByName[s.Name] = s
		}
	}
	newByName := map[string]Skill{}
	for _, s := range next.skills {
		newByName[s.Name] = s
		if prev, ok := oldByName[s.Name]; ok {
			if metaDigest(prev) != metaDigest(s) {
				res.Updated = append(res.Updated, s.Name)
			}
			continue
		}
		res.Added = append(res.Added, s.Name)
	}
	if old != nil {
		for _, s := range old.skills {
			if _, ok := newByName[s.Name]; !ok {
				res.Removed = append(res.Removed, s.Name)
			}
		}
	}
	sort.Strings(res.Added)
	sort.Strings(res.Removed)
	sort.Strings(res.Updated)
	res.Notice = c.renderNotice(res)
	return res
}

// renderNotice 渲染给模型的 user 侧短消息（替换目录）。
//
// 内容是**当前完整清单**（不是增量）：模型据此把旧清单整体换成新的。
// 与系统提示词里的清单同格式（同一个渲染器）——两处长得一样，模型不用
// 认第二种格式。
func (c *Catalog) renderNotice(res ReloadResult) string {
	var b strings.Builder
	b.WriteString("技能目录已更新")
	if len(res.Added) > 0 {
		b.WriteString("（新增 " + strings.Join(res.Added, "、") + "）")
	}
	if len(res.Removed) > 0 {
		b.WriteString("（移除 " + strings.Join(res.Removed, "、") + "）")
	}
	if len(res.Updated) > 0 {
		b.WriteString("（更新 " + strings.Join(res.Updated, "、") + "）")
	}
	b.WriteString("。以下是目前可加载的技能（替换此前的清单）：\n\n")
	if text := c.Render(); text != "" {
		b.WriteString(text)
	} else {
		b.WriteString("（当前没有可加载的技能）")
	}
	return b.String()
}

// Body 现读技能正文（调用时读，不驻留）。
//
// 现读的意义（S8.4-C，抄 DSH 的「目录/正文分离」）：改了 SKILL.md 立刻
// 生效——不用重扫、不用重启、不用盯文件。
//
// 预算在这里校验（不是装配期）：正文超限是**调用时报错**，不是「技能从
// 清单里消失」——后者会让模型以为技能不存在。
func (c *Catalog) Body(s Skill) (string, error) {
	if c == nil || c.reader == nil {
		return "", fmt.Errorf("没有技能读取器")
	}
	raw, err := c.reader.ReadFile(s.BodyPath)
	if err != nil {
		return "", fmt.Errorf("读技能 %q 的正文失败: %w", s.Name, err)
	}
	_, body, ok := splitFrontmatter(raw)
	if !ok {
		return "", fmt.Errorf("技能 %q 的文件已损坏（缺少 frontmatter）", s.Name)
	}
	if max := c.maxBodyBytes; max > 0 && len(body) > max {
		return "", fmt.Errorf("技能 %q 正文超预算（%d 字节，上限 %d）", s.Name, len(body), max)
	}
	return body, nil
}

// RenderContent 渲染技能正文块（进工具结果 / 用户手势展开）。
//
// 正文现读 + 渲染；读不到就报错（不返回半截正文——半截指令比没指令更糟）。
func (c *Catalog) RenderContent(s Skill) (string, error) {
	body, err := c.Body(s)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("<%s name=%q>\n%s\n</%s>",
		ContentTag, s.Name, escapeTag(body, ContentTag), ContentTag), nil
}

// digestOf 算目录摘要（技能集合的稳定指纹）。
func digestOf(skills []Skill) string {
	parts := make([]string, 0, len(skills))
	for _, s := range skills {
		parts = append(parts, metaDigest(s))
	}
	sort.Strings(parts)
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// metaDigest 算单个技能的元数据指纹（正文不进摘要）。
func metaDigest(s Skill) string {
	tools := append([]string(nil), s.AllowedTools...)
	sort.Strings(tools)
	return strings.Join([]string{
		s.Name,
		s.Description,
		s.WhenToUse,
		fmt.Sprintf("%t", s.DisableModelInvocation),
		strings.Join(tools, ","),
		s.Dir,
	}, "\x00")
}
