package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/puras/mos/tool"
)

// TaskParams 是子代理工具的参数（模型看到的 schema）。
//
// 只有两个参数：**要干的活**（必填）+ **附加上下文**（可选）。刻意不给
// 「用哪个子代理」这种参数——一个子代理就是一个工具，模型点名调用哪个
// 工具就派哪个子代理（agents-as-tools；DSH 是单工具 + 枚举参数，MoS 取
// 前者：工具面更直白，模型不用猜参数）。
type TaskParams struct {
	// Task 是要交代给子代理的任务。
	Task string `json:"task" mos:"desc=要交给子代理的任务（说清目标、范围与验收标准）,required=true"`
	// Context 是附加上下文（文件路径、代码片段、报错信息等）。
	Context string `json:"context,omitempty" mos:"desc=附加上下文（文件路径、代码片段、报错信息等，可空）"`
}

// ToolOptions 是子代理工具的行为选项。
type ToolOptions struct {
	// Timeout 是一次子代理调用的上限（0 = 不限）。
	//
	// 工具级超时（同 MCP 的 callTimeout 口径）：超时**不是**父轮次的错误，
	// 而是一段 `status: timeout` 的正常回执——父模型看得见「这个子代理
	// 没跑完」，可以改小任务重试。
	Timeout time.Duration
}

// Tool 是「一个子代理 = 一个工具」（2026-11-06 拍板：子代理 = 普通工具）。
//
// 它是**普通工具**：走同一套注册表、GuardBus、Policy、审批与
// tool/call / tool/result 流水线——子代理不另开一条道（PI / DSH 共识）。
//
// **独占**（不实现 tool.ConcurrencySafe）：子代理内部要发模型请求，父轮次
// 等它跑完（同步交付），并发跑多个子代理会同时抢模型配额——保守处理
// （tool 包的并发是显式 opt-in，不实现即独占）。
type Tool struct {
	def    Definition
	cat    *Catalog
	runner Runner
	opts   ToolOptions
}

// NewTool 造一个子代理工具（def 是声明，cat 提供指令正文的现读，runner 是
// 宿主注入的执行体）。
func NewTool(def Definition, cat *Catalog, runner Runner, opts ToolOptions) *Tool {
	return &Tool{def: def, cat: cat, runner: runner, opts: opts}
}

// Name 返回工具名（`subagent__<name>`）。
func (t *Tool) Name() string { return ToolName(t.def.Name) }

// Description 返回给模型看的说明：声明的描述 + 回执语义（自述要复核）。
func (t *Tool) Description() string {
	desc := strings.TrimSpace(t.def.Description)
	if desc == "" {
		desc = "子代理 " + t.def.Name
	}
	return desc + "（子代理；回执 {status, summary, findings}：summary 是子代理自述、findings 是待复核的线索，结论由你判断）"
}

// Parameters 返回参数结构体的零值示例（框架据此反射生成 JSON Schema）。
func (t *Tool) Parameters() any { return &TaskParams{} }

// Execute 跑一次子代理，把结果投影成**正常工具结果**。
//
// 错误分层（框架层 vs 子代理层）：
//   - **框架层错误**（返回 Go error）：参数不是 JSON、task 为空、定义文件
//     读不出来、没有可用的 Runner——这些是「这次调用压根没跑起来」，框架
//     把它标成失败，模型看得见错误文本、可自纠；
//   - **子代理层失败**（返回正常回执）：子代理跑挂了、超时了、回执不合法
//     ——父轮次**不**因此失败，父模型拿到 `status: error/timeout` +
//     原因说明，可以改小任务重试或换个子代理（同 §3.1「工具报错是结果」）。
//
// 超时与取消：ctx 透传给执行体（父轮次取消 → 子代理跟着取消）；执行体不
// 理会 ctx 时，超时**照样**返回（执行体在后台 goroutine 里跑，工具不等它），
// 残留 goroutine 由执行体自己负责退出。
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (any, error) {
	params, err := tool.Parse[TaskParams](args)
	if err != nil {
		return nil, fmt.Errorf("subagent: 解析参数: %w", err)
	}
	if strings.TrimSpace(params.Task) == "" {
		return nil, errors.New("subagent: task 不能为空")
	}
	if t.runner == nil {
		return nil, errors.New("subagent: 没有可用的 Runner")
	}
	if t.cat == nil {
		return nil, errors.New("subagent: 没有可用的清单")
	}
	spec, err := t.cat.BuildSpec(t.def, params.Task, params.Context)
	if err != nil {
		return nil, fmt.Errorf("subagent: 读取定义失败: %w", err)
	}

	runCtx := ctx
	if t.opts.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, t.opts.Timeout)
		defer cancel()
	}

	type outcome struct {
		res Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := t.runner.Run(runCtx, spec)
		ch <- outcome{res: res, err: err}
	}()

	select {
	case o := <-ch:
		if o.err != nil {
			return t.failure(o.err), nil
		}
		if err := o.res.Validate(); err != nil {
			return Result{
				Status:  StatusError,
				Summary: o.res.Summary,
				Error:   fmt.Sprintf("子代理回执不合法: %v", err),
			}, nil
		}
		return o.res, nil
	case <-runCtx.Done():
		return t.failure(runCtx.Err()), nil
	}
}

// failure 把执行错误投影成正常回执（父轮次不该因子代理失败而失败）。
func (t *Tool) failure(err error) Result {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Result{
			Status: StatusTimeout,
			Error:  fmt.Sprintf("子代理超时（上限 %s）", t.opts.Timeout),
		}
	case errors.Is(err, context.Canceled):
		return Result{Status: StatusError, Error: "子代理已取消"}
	default:
		return Result{Status: StatusError, Error: fmt.Sprintf("子代理执行失败: %v", err)}
	}
}

// Tools 为清单里的每个子代理造一个工具（名字 `subagent__<name>`）。
//
// 顺序按清单顺序（确定性，利于 prompt cache）。
func (c *Catalog) Tools(runner Runner, opts ToolOptions) []tool.Tool {
	if c == nil {
		return nil
	}
	defs := c.All()
	out := make([]tool.Tool, 0, len(defs))
	for _, d := range defs {
		out = append(out, NewTool(d, c, runner, opts))
	}
	return out
}
