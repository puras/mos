// stage6.go —— 第 6 站：装配与治理（seam / config / router / telemetry）。
//
// 学习目标：理解平台级扩展点——
//
//	seam.SeamRegistry 组件按 (kind, name) 注册工厂，装配层按名解析；
//	config 包凭 YAML + 注册表即可装配出可运行智能体（YAML 即装配）；
//	router.HybridRouter 规则硬路由优先 + LLM 分类兜底选驱动；
//	telemetry.Recorder 内存 span 树（driver.turn / llm.request / tool.execute）。
//
//	go run . -stage 6                # 默认 REPL 走 YAML 装配的 assistant
//	/spans                           # 打印本次运行的 span 树
//	/route 先规划：调研 temp 目录再写报告   # 规则命中 plan-execute
//	/route 帮我写个冒泡排序            # 规则未命中 → LLM 分类兜底（react）
//
// 观察点：换驱动只需改 agentcli.yaml 的 driver 字段；/route 打印选中驱动的
// 具体类型（规则优先于分类器）；span 树与事件流的对应关系。
package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/puras/mos/agent/config"
	"github.com/puras/mos/agent/driver"
	"github.com/puras/mos/agent/router"
	"github.com/puras/mos/agent/seam"
	"github.com/puras/mos/agent/telemetry"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/anthropic"
	"github.com/puras/mos/llm/openai"
	"github.com/puras/mos/planner"
)

const configFile = "agentcli.yaml"

func runStage6(model llm.ChatModel, protocol string) *harness {
	// ---- seam + config：注册 provider，凭 YAML 装配（主 REPL 走这条路）----
	reg := seam.New()
	reg.Register(seam.KindModel, "openai", func() any {
		m, err := openai.New(llm.Config{
			"api_key":  os.Getenv("OPENAI_API_KEY"),
			"base_url": os.Getenv("OPENAI_BASE_URL"),
			"model":    os.Getenv("OPENAI_MODEL"),
		})
		check(err)
		return m
	})
	reg.Register(seam.KindModel, "anthropic", func() any {
		m, err := anthropic.New(llm.Config{
			"api_key":  os.Getenv("ANTHROPIC_API_KEY"),
			"base_url": os.Getenv("ANTHROPIC_BASE_URL"),
			"model":    os.Getenv("ANTHROPIC_MODEL"),
		})
		check(err)
		return m
	})
	// 工具用内置包（yaml 里 tools: [builtin]），策略用 default，均无需注册；
	// 自定义组件在此按 (seam.KindTool/KindPolicy, name) 注册即可被 YAML 引用。

	raw, err := os.ReadFile(configFile)
	check(err)
	asst, err := config.Assemble(reg, raw)
	check(err)

	// 遥测：内存 Recorder 挂到装配好的 RunContext 上（/spans 查看）。
	rec := telemetry.NewRecorder()
	asst.Agent().RT.Telemetry = rec

	sess := asst.NewSession()
	h := &harness{
		protocol: protocol,
		rt:       asst.Agent().RT,
		agent:    asst.Agent(),
		sess:     sess,
	}
	h.extraBanner = append(h.extraBanner,
		"装配: "+configFile+"（seam 注册表按名解析 → assistant.New）",
		"工具: "+strings.Join(asst.ToolNames(), ", "),
		"命令: /spans span 树；/route <任务> 混合路由",
	)

	// ---- router：规则硬路由优先 + LLM 分类兜底 ----
	rt, _ := baseRunContext(model)
	rt.PlanPlanner = planner.NewPlan(model)
	rt.Reflector = planner.NewReflector(model)
	rt.ScriptPlanner = planner.NewScript(model)
	hybrid := router.NewHybrid(
		router.WithRule("先规划", "plan-execute"),
		router.WithRule("反思", "reflection"),
		router.WithRule("脚本", "scripted"),
		router.WithClassifier(planner.NewClassifier(model)),
	)

	// /route 用 HybridRouter 选驱动并用它执行（裸实例在 Run 时惰性补默认装配）。
	h.on("/route", func(arg string) {
		if arg == "" {
			fmt.Println("用法: /route <任务>（试试以“先规划”“反思”“脚本”开头）")
			return
		}
		drv, err := hybrid.Select(context.Background(), arg, h.sess)
		if err != nil {
			fmt.Fprintf(os.Stderr, "路由失败: %v\n", err)
			return
		}
		fmt.Printf("选中驱动: %s\n", reflect.TypeOf(drv))
		ag := driver.New(rt, drv)
		out, err := ag.Run(context.Background(), h.sess, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "route run: %v\n", err)
			return
		}
		fmt.Println(out)
	})

	// /spans 打印遥测 span 树（只打根 span，子 span 缩进）。
	h.on("/spans", func(string) {
		spans := rec.Spans()
		if len(spans) == 0 {
			fmt.Println("（暂无 span；先跑一个任务）")
			return
		}
		fmt.Printf("span 树（共 %d 条记录）：\n", len(spans))
		for _, s := range spans {
			if s.Parent == nil {
				printSpan(s, 0)
			}
		}
	})
	return h
}

// printSpan 递归打印一条 span 及其子树。
func printSpan(s telemetry.SpanRecord, depth int) {
	dur := s.End.Sub(s.Start).Round(time.Millisecond)
	status := ""
	if s.Err != nil {
		status = " err=" + s.Err.Error()
	}
	fmt.Printf("%s%s  %s%s\n", strings.Repeat("  ", depth), s.Name, dur, status)
	for _, c := range s.Children {
		printSpan(*c, depth+1)
	}
}
