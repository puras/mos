package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/event"
	"github.com/puras/mos/agent/session"
	"github.com/puras/mos/tool"
)

// ActivateSkill 是技能激活工具（MoS Skills 强化 Task 6）：
// 模型经 activate_skill 工具触发一个技能，把正文 + references + 允许工具列表注入本次 Run。
// 工具调用流程：
//  1. 解析 name → 在 Skills 中查找匹配；
//  2. 读取 SKILL.md 正文（去掉 frontmatter）；
//  3. sess 注入：sess.AddTurnSkill(target) 累积 + 更新 sess.TurnToolSubset；
//  4. 经 ctx 取出装配层注入的 *session.Session → 写 skill/activate 事件（nil 时跳过）；
//  5. 返回 SkillActivationResult 给模型（body / references / allowed_tools）。
//
// 名称 / 描述 / 参数 schema：经 tool.Tool 接口暴露给模型（注册进 assistant.New 的工具集）。
// 事件写入失败 fail-fast 上抛（与 skill/load 一致；见 Session.Append 语义）。
type ActivateSkill struct {
	Skills []agent.Skill
}

// Name 实现 tool.Tool。
func (ActivateSkill) Name() string { return "activate_skill" }

// Description 实现 tool.Tool。
func (ActivateSkill) Description() string {
	return "按名激活一个技能：返回该技能的正文、references 路径与允许工具白名单。" +
		"激活在本次任务内持续生效（可继续激活其他技能，白名单取并集），也可直接使用返回的正文。"
}

// Parameters 实现 tool.Tool：仅 name 一个必填字段。
func (ActivateSkill) Parameters() any { return &activateSkillArgs{} }

// Sequential 实现 tool.SequentialTool：本工具改写共享会话状态
// （sess.TurnSkills / sess.TurnToolSubset 累积 + sess.Append 写事件日志），
// 模型一次返回多个 activate_skill 时并发执行会产生数据竞争（Seq 重复/丢失、
// 白名单切片撕裂——而白名单是安全控制）。声明串行让 execpath.ExecuteBatch
// 把整批回退为顺序执行，与 rt.AddFact 用 factMu 串行化 TaskState.Facts 同源。
func (ActivateSkill) Sequential() bool { return true }

// activateSkillArgs 是 activate_skill 工具参数。
type activateSkillArgs struct {
	Name string `json:"name" jsonschema:"required"`
}

// SkillActivationResult 是 activate_skill 工具返回值（同时作为 skill/activate 事件不重写 body）。
// 正文由工具直接返回给模型（不进 Event Log，避免大字段污染会话日志）。
type SkillActivationResult struct {
	Name         string   `json:"name"`
	Body         string   `json:"body"`
	References   []string `json:"references"`
	AllowedTools []string `json:"allowed_tools"`
}

// Execute 实现 tool.Tool。
func (t ActivateSkill) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	a, err := tool.Parse[activateSkillArgs](args)
	if err != nil {
		return nil, fmt.Errorf("activate_skill: %w", err)
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return nil, fmt.Errorf("activate_skill: name required")
	}
	var target *agent.Skill
	for i := range t.Skills {
		if t.Skills[i].Name == name {
			target = &t.Skills[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("activate_skill: skill %q not found", name)
	}

	body, _ := readSkillBody(target.Dir)

	// sess 注入：本次 Run 已激活 + 工具子集并集（替代旧 ctx 方案——tool.Registry.Call
	// 不返回 ctx，累积状态须放在可见对象 sess 上）。
	if sess := sessionFromCtx(ctx); sess != nil {
		agent.AddTurnSkill(sess, *target)
		// 写 skill/activate 事件（装配层经 WithSession 注入 sess；nil 时跳过事件）
		if err := sess.Append(event.SessionEventSkillActivate, event.MarshalData(event.SkillActivateData{
			Name:         target.Name,
			AllowedTools: target.AllowedTools,
			References:   target.References,
		})); err != nil {
			return nil, fmt.Errorf("activate_skill: 写 skill/activate 事件失败: %w", err)
		}
	}

	return SkillActivationResult{
		Name:         target.Name,
		Body:         body,
		References:   target.References,
		AllowedTools: target.AllowedTools,
	}, nil
}

// readSkillBody 读 SKILL.md 文件的正文部分（去掉 frontmatter）。
// 复用 agent.extractFrontmatter 的解析思路，但读的是正文而非 YAML。
func readSkillBody(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", err
	}
	s := string(b)
	if !strings.HasPrefix(strings.TrimSpace(s), "---") {
		return strings.TrimSpace(s), nil
	}
	rest := strings.TrimPrefix(strings.TrimSpace(s), "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return strings.TrimSpace(s), nil
	}
	return strings.TrimSpace(rest[end+4:]), nil
}

// sessionFromCtx 从 ctx 取 *session.Session（装配层注入；nil 时跳过事件）。
// 委托 agent.SessionFromCtx：key 定义在 agent 包内，driver（execpath）可注入。
func sessionFromCtx(ctx context.Context) *session.Session {
	return agent.SessionFromCtx(ctx)
}

