package agent

import (
	"context"
)

// 激活态 ctx 注入：本轮已激活的 Skill 列表 + 工具子集并集。
// 与 request_human_input 同样的"ctx 携带本轮瞬时状态"模式。

type activatedSkillsKey struct{}
type toolSubsetKey struct{}

// ActivatedSkillsFromCtx 返回 ctx 中累积的已激活 Skill 列表；无则返回 nil。
func ActivatedSkillsFromCtx(ctx context.Context) []Skill {
	v, _ := ctx.Value(activatedSkillsKey{}).([]Skill)
	return v
}

// WithActivatedSkills 把 Skill 列表注入 ctx。调用方负责累积（不复制）。
func WithActivatedSkills(ctx context.Context, sk []Skill) context.Context {
	return context.WithValue(ctx, activatedSkillsKey{}, sk)
}

// ToolSubsetFromCtx 返回 ctx 中累积的允许工具名并集；nil = 未限制。
func ToolSubsetFromCtx(ctx context.Context) []string {
	v, _ := ctx.Value(toolSubsetKey{}).([]string)
	return v
}

// WithToolSubset 把工具名并集注入 ctx；调用方负责并集去重。
func WithToolSubset(ctx context.Context, names []string) context.Context {
	return context.WithValue(ctx, toolSubsetKey{}, names)
}
