package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Status 是子代理回执的状态。
//
// 三档，不多不少（2026-11-06 拍板）：**子代理失败不是父轮次失败**——
// 失败也要回一段正常的工具结果，父模型看得见、可自纠（同 §3.1「工具报错
// 是结果」）。
type Status string

const (
	// StatusCompleted 表示子代理跑完了。
	StatusCompleted Status = "completed"
	// StatusError 表示子代理跑失败了（回执里带 Error 说明）。
	StatusError Status = "error"
	// StatusTimeout 表示子代理超时被掐（工具级超时，同 MCP 的 callTimeout）。
	StatusTimeout Status = "timeout"
)

// Valid 判断状态取值是否合法。
func (s Status) Valid() bool {
	switch s {
	case StatusCompleted, StatusError, StatusTimeout:
		return true
	}
	return false
}

// Finding 是子代理给出的一条线索（父模型**要自己复核**）。
//
// 按 NFR-5：子代理的自述**不进父的 TaskState**——它只是线索。Ref 指向可
// 核对的东西（文件路径 / 命令 / 行号），父模型拿它自己去验。
type Finding struct {
	// Ref 是可核对的位置（文件路径、命令、位置说明）。
	Ref string `json:"ref,omitempty"`
	// Summary 是一句话说明（**子代理自述**）。
	Summary string `json:"summary"`
}

// Result 是子代理的固定结构回执。
//
// 结构固定（不随子代理变）：父模型拿到的永远是同一形状的几样东西——状态、
// 一句自述、若干条线索。`summary` 是**子代理自述**，不是事实。
type Result struct {
	// Status 是执行状态。
	Status Status `json:"status"`
	// Summary 是子代理对结果的一句话总结（**自述**）。
	Summary string `json:"summary"`
	// Findings 是结构化线索（父模型要自己复核）。
	Findings []Finding `json:"findings,omitempty"`
	// Error 是失败原因（status = error / timeout 时非空）。
	Error string `json:"error,omitempty"`
}

// Validate 校验回执形状。
//
// fail-closed：状态不在三档里 → 回执不合法；status 不是 completed 却带空
// 错误说明 → 也算不合法（父模型得知道为什么失败）。
func (r Result) Validate() error {
	if !r.Status.Valid() {
		return fmt.Errorf("status %q 不是合法取值（completed / error / timeout）", r.Status)
	}
	if r.Status != StatusCompleted && strings.TrimSpace(r.Error) == "" {
		return fmt.Errorf("status %q 必须带 error 说明", r.Status)
	}
	for i, f := range r.Findings {
		if strings.TrimSpace(f.Summary) == "" {
			return fmt.Errorf("findings[%d] 缺 summary", i)
		}
	}
	return nil
}

// Spec 是一次子代理调用的输入。
//
// 执行体（宿主注入的 Runner）拿它建子 Agent：指令正文 + 白名单 + 模型覆盖
// + 这次要干的活。
type Spec struct {
	// Name 是子代理名（工具名 = subagent__<name>）。
	Name string `json:"name"`
	// Task 是父模型交代的任务。
	Task string `json:"task"`
	// Context 是附加上下文（可选；父模型给的额外材料）。
	Context string `json:"context,omitempty"`
	// Instructions 是子代理的指令正文（现读；见 Catalog.BuildSpec）。
	Instructions string `json:"instructions"`
	// Tools 是**声明白名单**（原始声明）。实际工具面由执行体收窄：
	// 声明白名单 ∩ 父可见工具集 ∩ 宿主 policy（deny 优先）。
	Tools []string `json:"tools,omitempty"`
	// Model 是模型覆盖（空 = 用父模型）。
	Model string `json:"model,omitempty"`
}

// Runner 是子代理的**执行体**（宿主注入）。
//
// 框架不自己造 Agent（治理规则 5 + 避免根包与本包成环）：本包只定义接缝，
// 怎么造子 Agent（独立 session.Session、白名单子注册表、模型覆盖、子代理
// 内部的工具调用怎么走 execpath）由装配层实现。
//
// 契约：
//   - **同步**：Run 跑完才返回（2026-11-06 拍板：MoS 没有运行期消息回注
//     通道，抄 DSH 的「异步 id + 另行通知」要先造一条通道）；
//   - **失败不是错误**：子代理跑挂了，Run 也要**返回**一个 status=error 的
//     Result（父轮次不该因此失败）——返回 error 只表示「连子代理都没跑起来」
//     （例如定义读不出来）；
//   - **取消透传**：ctx 取消 → 子代理跟着取消（DSH 的 signal 同义）。
type Runner interface {
	// Run 跑一次子代理，返回固定结构的回执。
	Run(ctx context.Context, spec Spec) (Result, error)
}

// RunnerFunc 是 Runner 的函数式实现（宿主不想写结构体时用）。
type RunnerFunc func(ctx context.Context, spec Spec) (Result, error)

// Run 实现 Runner。
func (f RunnerFunc) Run(ctx context.Context, spec Spec) (Result, error) {
	if f == nil {
		return Result{}, errors.New("没有可用的 Runner")
	}
	return f(ctx, spec)
}
