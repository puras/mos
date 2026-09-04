// Package config 提供声明式配置加载（架构 v2 §1.1/§3.5）：YAML 配置 →
// 组件按名解析（SeamRegistry）→ 组装好的可运行智能体（assistant）。
// 平台（如 SaaS 智能体平台）注册 provider 后，仅凭 YAML 即可完成装配。
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/seam"
	"github.com/puras/mos/assistant"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/planner"
	"github.com/puras/mos/tool"
	"github.com/puras/mos/tool/builtin"
)

// Config 是 YAML 声明式装配配置（字段对齐 assistant.Config 的核心概念；
// 组件类目见架构 v2 §3.5 按名选组件）。
type Config struct {
	// Driver 驱动名（react / plan-execute / reflection，缺省 react）。
	Driver string `yaml:"driver"`
	// Model 模型 provider 名（SeamRegistry 必填，必需）。
	Model string `yaml:"model"`
	// Tools 工具集：["builtin"] = 内置工具包（缺省）；其余名称走 seam tool provider。
	Tools []string `yaml:"tools"`
	// Policy 安全策略 provider 名（缺省 default = 无额外限制）。
	Policy string `yaml:"policy"`
	// PlanPlanner Plan&Execute 计划生成器 provider 名（Driver=plan-execute 时使用）。
	PlanPlanner string `yaml:"plan_planner"`
	// Reflector Reflection 评审器 provider 名（Driver=reflection 时必需）。
	Reflector string `yaml:"reflector"`
	// ScriptPlanner 脚本编排规划器 provider 名（Driver=scripted 时必需；opt-in）。
	ScriptPlanner string `yaml:"script_planner"`
	// SystemPrompt 系统引导词。
	SystemPrompt string `yaml:"system_prompt"`
	// MaxTurns 可选保险丝（>0 生效；0 不限制）。
	MaxTurns int `yaml:"max_turns"`
	// ContextBudget 模型上下文 token 预算（>0 启用利用率监控）。
	ContextBudget int `yaml:"context_budget"`
	// DoomLoopMaxRepeat Doom Loop 检测阈值（>0 启用）。
	DoomLoopMaxRepeat int `yaml:"doom_loop_max_repeat"`
	// ProjectDocDir 项目指令目录（自动构建默认组装器）。
	ProjectDocDir string `yaml:"project_doc_dir"`
	// SkillsDir 技能目录（自动构建默认组装器）。
	SkillsDir string `yaml:"skills_dir"`
	// WorkspaceRoot 文件工具 jail 根目录（Task 10：必填；缺省回退到进程 cwd）。
	// 空值由 assistant.New 校验拒绝；本字段为空时 Assemble 自动用 cwd 兜底，
	// 避免单元测试等"无项目上下文"场景无法启动。
	WorkspaceRoot string `yaml:"workspace_root"`
}

// Load 解析 YAML 配置（未做组件解析，纯结构校验）。
func Load(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}
	return &cfg, nil
}

// Assemble 按 YAML 配置装配智能体：SeamRegistry 按名解析模型/驱动/策略等组件，
// 返回可运行的 *assistant.Assistant。缺省 Driver=react、Tools=内置工具包、
// Policy=default；未知驱动名 / 缺少模型 / 组件未注册时返回错误（fail-fast）。
func Assemble(reg *seam.SeamRegistry, raw []byte) (*assistant.Assistant, error) {
	cfg, err := Load(raw)
	if err != nil {
		return nil, err
	}
	return AssembleConfig(reg, cfg)
}

// AssembleConfig 按已解析的 Config 装配智能体（供编程式配置复用）。
func AssembleConfig(reg *seam.SeamRegistry, cfg *Config) (*assistant.Assistant, error) {
	if reg == nil {
		return nil, fmt.Errorf("config: nil seam registry")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("config: model is required")
	}

	model, err := resolveModel(reg, cfg.Model)
	if err != nil {
		return nil, err
	}
	tools, err := resolveTools(reg, cfg.Tools)
	if err != nil {
		return nil, err
	}
	policy, err := resolvePolicy(reg, cfg.Policy)
	if err != nil {
		return nil, err
	}
	planPlanner, err := resolvePlanPlanner(reg, cfg.PlanPlanner)
	if err != nil {
		return nil, err
	}
	reflector, err := resolveReflector(reg, cfg.Reflector)
	if err != nil {
		return nil, err
	}
	scriptPlanner, err := resolveScriptPlanner(reg, cfg.ScriptPlanner)
	if err != nil {
		return nil, err
	}

	workspaceRoot := cfg.WorkspaceRoot
	if workspaceRoot == "" {
		var cwdErr error
		workspaceRoot, cwdErr = os.Getwd()
		if cwdErr != nil {
			return nil, fmt.Errorf("config: WorkspaceRoot 缺省且无法获取 cwd: %w", cwdErr)
		}
	}
	ac := assistant.Config{
		Driver:            cfg.Driver,
		PlanPlanner:       planPlanner,
		Reflector:         reflector,
		ScriptPlanner:     scriptPlanner,
		SystemPrompt:      cfg.SystemPrompt,
		MaxTurns:          cfg.MaxTurns,
		ContextBudget:     cfg.ContextBudget,
		DoomLoopMaxRepeat: cfg.DoomLoopMaxRepeat,
		ProjectDocDir:     cfg.ProjectDocDir,
		SkillsDir:         cfg.SkillsDir,
		Policy:            policy,
		FileAccess:        assistant.FileAccessConfig{WorkspaceRoot: workspaceRoot},
	}
	return assistant.New(model, tools, ac)
}

// resolveModel 按名解析模型 provider。
func resolveModel(reg *seam.SeamRegistry, name string) (llm.ChatModel, error) {
	v, err := reg.Resolve(seam.KindModel, name)
	if err != nil {
		return nil, fmt.Errorf("config: model: %w", err)
	}
	m, ok := v.(llm.ChatModel)
	if !ok {
		return nil, fmt.Errorf("config: model provider %q is not llm.ChatModel (%T)", name, v)
	}
	return m, nil
}

// resolveTools 组装工具集：缺省 = 内置工具包；"builtin" 展开；其余按名走 seam。
func resolveTools(reg *seam.SeamRegistry, names []string) ([]tool.Tool, error) {
	if len(names) == 0 {
		return builtin.Default(), nil
	}
	var out []tool.Tool
	for _, name := range names {
		if name == "builtin" {
			out = append(out, builtin.Default()...)
			continue
		}
		v, err := reg.Resolve(seam.KindTool, name)
		if err != nil {
			return nil, fmt.Errorf("config: tool: %w", err)
		}
		t, ok := v.(tool.Tool)
		if !ok {
			return nil, fmt.Errorf("config: tool provider %q is not tool.Tool (%T)", name, v)
		}
		out = append(out, t)
	}
	return out, nil
}

// resolvePolicy 按名解析安全策略；缺省 default = 无额外限制。
func resolvePolicy(reg *seam.SeamRegistry, name string) (driver.Policy, error) {
	if name == "" {
		name = "default"
	}
	if name == "default" {
		return assistant.NewDefaultPolicy(nil, ""), nil
	}
	v, err := reg.Resolve(seam.KindPolicy, name)
	if err != nil {
		return nil, fmt.Errorf("config: policy: %w", err)
	}
	p, ok := v.(driver.Policy)
	if !ok {
		return nil, fmt.Errorf("config: policy provider %q is not driver.Policy (%T)", name, v)
	}
	return p, nil
}

// resolvePlanPlanner 按名解析计划生成器（可选，未配置返回 nil）。
func resolvePlanPlanner(reg *seam.SeamRegistry, name string) (planner.PlanPlanner, error) {
	if name == "" {
		return nil, nil
	}
	v, err := reg.Resolve(seam.KindPlanner, name)
	if err != nil {
		return nil, fmt.Errorf("config: plan_planner: %w", err)
	}
	p, ok := v.(planner.PlanPlanner)
	if !ok {
		return nil, fmt.Errorf("config: plan_planner provider %q is not planner.PlanPlanner (%T)", name, v)
	}
	return p, nil
}

// resolveReflector 按名解析评审器（可选，未配置返回 nil）。
func resolveReflector(reg *seam.SeamRegistry, name string) (planner.Reflector, error) {
	if name == "" {
		return nil, nil
	}
	v, err := reg.Resolve(seam.KindReflector, name)
	if err != nil {
		return nil, fmt.Errorf("config: reflector: %w", err)
	}
	r, ok := v.(planner.Reflector)
	if !ok {
		return nil, fmt.Errorf("config: reflector provider %q is not planner.Reflector (%T)", name, v)
	}
	return r, nil
}

// resolveScriptPlanner 按名解析脚本编排规划器（可选，未配置返回 nil）。
func resolveScriptPlanner(reg *seam.SeamRegistry, name string) (planner.ScriptPlanner, error) {
	if name == "" {
		return nil, nil
	}
	v, err := reg.Resolve(seam.KindPlanner, name)
	if err != nil {
		return nil, fmt.Errorf("config: script_planner: %w", err)
	}
	p, ok := v.(planner.ScriptPlanner)
	if !ok {
		return nil, fmt.Errorf("config: script_planner provider %q is not planner.ScriptPlanner (%T)", name, v)
	}
	return p, nil
}
