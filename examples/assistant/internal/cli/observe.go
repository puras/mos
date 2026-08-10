package cli

import (
	"fmt"
	"strings"
	"time"

	"hegq.net/mos/pkg/llm"
)

// logSink 模型调用日志的输出口：TUI 模式下由界面接管（通道转发），否则回退到 stdout。
// 展示路由能力属于应用层，框架只负责通过 WithLogging 上报 CallResult。
var logSink func(string)

// emitLog 输出一行模型调用日志：优先交给 logSink（TUI 界面），否则直接打印到 stdout。
func emitLog(line string) {
	if logSink != nil {
		logSink(line)
		return
	}
	fmt.Println(line)
}

// logModelCall 返回 llm.WithLogging 的回调：每次模型调用结束后打印发起概要 + 结果概要。
// 展示层职责（模型名/地址/用量格式化）留在应用层，框架只负责包装时机与上下文。
func logModelCall(name, endpoint string) func(llm.CallResult) {
	var call int
	return func(r llm.CallResult) {
		call++
		tag := cyan(fmt.Sprintf("[调用%d]", call))
		stream := ""
		if r.Stream {
			stream = "（流式）"
		}
		emitLog("  " + tag + " " + describeModel(name, endpoint) + " · " +
			dim(fmt.Sprintf("消息 %d 条 · 工具 %d 个%s", len(r.Req.Messages), len(r.Req.Tools), stream)))
		elapsed := r.Elapsed.Round(time.Millisecond)
		if r.Err != nil {
			emitLog("  " + tag + " " + red("失败 "+r.Err.Error()) + " · " + dim("耗时 "+elapsed.String()))
			return
		}
		emitLog("  " + tag + " " + green("完成") + " · " + yellow(usageLine(r.Resp.Usage)) + " · " + dim("耗时 "+elapsed.String()))
	}
}

// describeModel 返回 "模型名 · 请求地址" 描述段，地址为空时省略。
func describeModel(name, endpoint string) string {
	if endpoint == "" {
		return fmt.Sprintf("模型 %s", name)
	}
	return fmt.Sprintf("模型 %s · 地址 %s", name, endpoint)
}

// usageLine 返回 token 用量打印行，含缓存/推理/音频明细（有值时）。
func usageLine(u llm.Usage) string {
	line := fmt.Sprintf("token %d/%d/%d", u.PromptTokens, u.CompletionTokens, u.TotalTokens)
	d := u.Details
	var parts []string
	if d.Input.CacheRead > 0 {
		parts = append(parts, fmt.Sprintf("缓存读 %d", d.Input.CacheRead))
	}
	if d.Input.CacheWrite > 0 {
		parts = append(parts, fmt.Sprintf("缓存写 %d", d.Input.CacheWrite))
	}
	if d.Output.Reasoning > 0 {
		parts = append(parts, fmt.Sprintf("推理 %d", d.Output.Reasoning))
	}
	if d.Input.Audio > 0 {
		parts = append(parts, fmt.Sprintf("音频入 %d", d.Input.Audio))
	}
	if d.Output.Audio > 0 {
		parts = append(parts, fmt.Sprintf("音频出 %d", d.Output.Audio))
	}
	if len(parts) > 0 {
		line += " · " + strings.Join(parts, " · ")
	}
	return line
}
