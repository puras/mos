package mos

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/puras/mos/agent/skill"
	"github.com/puras/mos/agent/subagent"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/session"
	"github.com/puras/mos/tool"
)

// DefaultSubAgentTimeout 是单次子代理调用的默认上限（5 分钟）。
//
// 子代理跑的是**一整轮智能体循环**（多次模型请求 + 工具调用），比单次
// 工具调用慢得多，所以不能套用 MCP 的 60s。但也不能不限：父轮次会被子
// 代理占住（同步交付），网关半开时父轮次就永远挂着——宿主拿不到 turn/end。
const DefaultSubAgentTimeout = 5 * time.Minute

// SubAgentConfig 是子代理装配参数（PRD FR-D3）。
//
// 零值可用：Dirs 空 = 没有子代理目录（不装子代理）；Reader 空 = 读本地文件。
type SubAgentConfig struct {
	// Dirs 是子代理目录列表（按优先级从高到低）。
	//
	// 顺序即优先级：同名子代理**先到先得**（后来的被遮蔽并出诊断）。
	// 每个目录下：目录自己含 AGENT.md 就是子代理根，否则它的每个直接子目录
	// （含 AGENT.md）是一个子代理。
	//
	// 目录列表本身就是**宿主声明的授权边界**（同 Skills）。
	Dirs []string
	// Reader 是读子代理文件与列目录的能力（nil = 读本地文件）。
	Reader subagent.Reader
	// MaxCatalogBytes 是清单渲染的字节上限（≤0 = 8KB）。超预算时先丢描述、
	// 再丢条目，丢了多少如实写进末尾提示。
	MaxCatalogBytes int
	// Timeout 是单次子代理调用的上限（≤0 = DefaultSubAgentTimeout）。
	//
	// 超时返回 `status: timeout` 的正常回执——父轮次不失败，父模型看得见
	// 「这个子代理没跑完」，可以改小任务重试。
	Timeout time.Duration
	// Runner 是自定义执行体（nil = 用默认实现）。
	//
	// 默认实现：独立子会话 + 白名单子注册表 + 模型覆盖（见 runSubagent）。
	// 宿主自备时，子代理的会话、工具面、模型全归宿主管——框架只负责把
	// 声明、任务与回执结构接起来。
	Runner subagent.Runner
}

// subagentParentKey 是「父会话」的 ctx 键（私有类型，避免撞键）。
//
// 子代理要知道父会话，才能派生出可辨认的子会话 id（宿主查子会话日志时
// 看得出它属于哪一轮）。
type subagentParentKey struct{}

// withSubagentParent 把父会话挂进 ctx（子代理执行时取用）。
func withSubagentParent(ctx context.Context, sess *session.Session) context.Context {
	if sess == nil {
		return ctx
	}
	return context.WithValue(ctx, subagentParentKey{}, sess)
}

// subagentParentFrom 取父会话（没有就是 nil）。
func subagentParentFrom(ctx context.Context) *session.Session {
	sess, _ := ctx.Value(subagentParentKey{}).(*session.Session)
	return sess
}

// runSubagent 是默认的子代理执行体：跑一次**独立子会话**。
//
// 形态（§3.18 拍板）：
//   - **独立子会话**：新的 session.Session（同一持久化后端）——子代理的
//     中间推理与工具调用留在**子会话日志**里，不进父上下文；
//   - **白名单子注册表**：子代理拿的是新注册表，只有「声明白名单 ∩ 父可见
//     工具集」里的工具（deny 优先）；子代理工具与 tool_search **不进**
//     子注册表（深度 0：不许再委派；工具面不经 search：不许自己扩容）；
//   - **模型覆盖**：声明了 model 就用目录里那个，否则跟父的当前模型；
//   - **同步**：父轮次等它跑完（没有运行期消息回注通道，异步要先造通道）。
//
// 子代理内部的工具调用仍走 execpath / policy / guard / approval——父的
// 钩子与守卫原样继承（rt 是浅拷贝），所以子代理不能绕过父的策略。
func (a *Agent) runSubagent(ctx context.Context, spec subagent.Spec) (subagent.Result, error) {
	if a == nil {
		return subagent.Result{}, fmt.Errorf("mos: 智能体未装配")
	}
	// 子注册表：白名单 ∩ 父可见集，扣掉子代理工具与 tool_search。
	child, dropped := subagent.ChildRegistry(a.tools, spec.Tools)
	// 子模型：声明了覆盖就用目录里那个，否则跟父的当前模型。
	model, err := a.subagentModel(spec.Model)
	if err != nil {
		return subagent.Result{}, err
	}
	// 子会话：独立 id + 同一持久化后端（宿主可查子会话日志）。
	childSess := session.New(a.subagentSessionID(ctx, spec.Name), a.persistence)
	// 子运行上下文：继承父的钩子 / 守卫 / 审批 / 压缩，换工具面与提示词。
	rt := *a.rt
	rt.Planner = planner.NewReAct(model)
	rt.Tools = child
	rt.SystemPrompt = a.subagentSystemPrompt(spec, child)
	rt.Stream = false
	rt.OnChunk = nil

	out, err := a.driver.Run(ctx, &rt, childSess, subagentTask(spec))
	if err != nil {
		return subagent.Result{Status: subagent.StatusError, Error: fmt.Sprintf("子代理执行失败: %v", err)}, nil
	}
	if len(dropped) > 0 {
		// 被挡掉的声明项照实说（父策略挡掉的工具，子代理拿不到）。
		out = strings.TrimSpace(out)
		out += "\n\n[注意] 声明里这些工具本次不可用（被父策略挡掉或不允许子代理继承）：" + strings.Join(dropped, "、")
	}
	return parseSubagentReceipt(out), nil
}

// subagentModelProbe 返回模型存在性探针（宿主模型目录）。
//
// 单模型模式没有目录 → 任何模型 id 都不存在 → 子代理声明了模型就装配失败
// （不静默降级到父模型：降级等于让模型以为自己在用 A、实际在用 B）。
func subagentModelProbe(cat *modelCatalog) subagent.ModelProbe {
	return func(model string) bool {
		if cat == nil {
			return false
		}
		_, ok := cat.lookup(model)
		return ok
	}
}

// subagentParentProfile 描述父代理的形状（拆分判据的参照）。
func (a *Agent) subagentParentProfile() subagent.ParentProfile {
	profile := subagent.ParentProfile{Tools: a.visibleNames()}
	if a.catalog != nil {
		profile.Model = a.catalog.currentID()
	}
	return profile
}

// subagentModel 取子代理该用的模型。
//
// 声明了覆盖但目录里没有 → 报错（装配期已查过一次，这里是运行期兜底：
// 模型目录可能在装配后被换掉）。没声明 → 跟父的当前模型。
func (a *Agent) subagentModel(override string) (llm.ChatModel, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		if a.catalog != nil {
			return a.catalog.shell, nil // 转发壳：跟父的当前模型
		}
		if a.baseModel != nil {
			return a.baseModel, nil
		}
		return nil, fmt.Errorf("mos: 子代理没有可用模型")
	}
	if a.catalog == nil {
		return nil, fmt.Errorf("mos: 子代理声明了模型 %q，但宿主是单模型模式（没有模型目录）", override)
	}
	m, ok := a.catalog.lookup(override)
	if !ok {
		return nil, fmt.Errorf("mos: 子代理声明的模型 %q 不在模型目录里", override)
	}
	return m, nil
}

// subagentSessionID 派生子会话 id（父会话 id + 子代理名 + 序号）。
func (a *Agent) subagentSessionID(ctx context.Context, name string) string {
	seq := a.subagentSeq.Add(1)
	parent := ""
	if sess := subagentParentFrom(ctx); sess != nil {
		parent = sess.ID
	}
	if parent == "" {
		return fmt.Sprintf("sub-%s-%d", name, seq)
	}
	return fmt.Sprintf("%s-sub-%s-%d", parent, name, seq)
}

// subagentSystemPrompt 装配子代理的系统提示词。
//
// 三段：人设（AGENT.md 正文，现读）→ 项目约定（父的项目上下文，子代理
// 也在这个项目里干活）→ 回执格式（固定结构，父模型按它复核）。
//
// **不复用父的提示词**：父的提示词里有技能清单、MCP 服务器摘要——子代理的
// 工具面里没有那些工具，列出来只会骗模型。技能清单在子代理确实带 skill
// 工具时单独补上。
func (a *Agent) subagentSystemPrompt(spec subagent.Spec, child *tool.Registry) string {
	var parts []string
	if inst := strings.TrimSpace(spec.Instructions); inst != "" {
		parts = append(parts, inst)
	} else {
		parts = append(parts, fmt.Sprintf("你是子代理 %s。", spec.Name))
	}
	if ctxText := strings.TrimSpace(a.contextText); ctxText != "" {
		parts = append(parts, ctxText)
	}
	if _, ok := child.Get(skill.ToolName); ok && a.skillsText != "" {
		parts = append(parts, a.skillsText)
	}
	parts = append(parts, subagentReceiptInstruction)
	return strings.Join(parts, "\n\n")
}

// subagentReceiptInstruction 是子代理的**回执格式**约定（进子代理提示词）。
//
// 固定结构（父模型按它复核）：summary 是子代理自述，findings 是可核对的线索。
const subagentReceiptInstruction = `完成之后只回答一段 JSON（不要加别的文字）：
{"summary": "一句话结论（你的自述，调用方会复核）", "findings": [{"ref": "可核对的位置（文件:行、命令、标识符）", "summary": "这条线索说明什么"}]}
findings 没有就写空数组。`

// subagentTask 拼子代理这一轮的输入（任务 + 附加上下文）。
func subagentTask(spec subagent.Spec) string {
	task := strings.TrimSpace(spec.Task)
	if ctx := strings.TrimSpace(spec.Context); ctx != "" {
		return task + "\n\n[附加上下文]\n" + ctx
	}
	return task
}

// parseSubagentReceipt 把子代理的最终回答投影成固定结构回执。
//
// 子代理被要求回答 JSON；答的不是 JSON（模型不听话、或者只说了半句话）
// 就**整段当 summary**——回执的形状由框架保证（父模型永远拿到同一形状），
// 内容是子代理的自述（NFR-5：父模型要自己复核）。
func parseSubagentReceipt(text string) subagent.Result {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return subagent.Result{Status: subagent.StatusCompleted, Summary: "（子代理没有给出结论）"}
	}
	var probe struct {
		Summary  string             `json:"summary"`
		Findings []subagent.Finding `json:"findings"`
	}
	if body, ok := stripJSONFence(trimmed); ok {
		if err := json.Unmarshal([]byte(body), &probe); err == nil && (probe.Summary != "" || len(probe.Findings) > 0) {
			return subagent.Result{
				Status:   subagent.StatusCompleted,
				Summary:  probe.Summary,
				Findings: probe.Findings,
			}
		}
	}
	return subagent.Result{Status: subagent.StatusCompleted, Summary: trimmed}
}

// stripJSONFence 剥掉模型爱加的 ```json 代码围栏（剥不掉就原样返回）。
func stripJSONFence(text string) (string, bool) {
	if !strings.HasPrefix(text, "```") {
		return text, true
	}
	body := strings.TrimPrefix(text, "```")
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		// 第一行是语言标记（```json / ```JSON / ```）。
		body = body[i+1:]
	}
	body = strings.TrimSuffix(strings.TrimSpace(body), "```")
	return strings.TrimSpace(body), true
}
