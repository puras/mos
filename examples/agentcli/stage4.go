// stage4.go —— 第 4 站：上下文工程五件套（模型每轮"看到什么"由谁决定）。
//
// 学习目标：理解五个上下文扩展点的分工——
//
//	SystemAssembler 槽位式系统提示词（每轮重组，固定槽位利于 prompt cache）
//	ResultTruncator 工具结果截断瘦身（超限落盘逃生通道）
//	Compactor       fresh-context 压缩（摘要取自 TaskState，非 LLM 总结）
//	Auditor         审计门（只有只读工具输出才能写入 Facts）
//	Skills          技能清单（SKILL.md frontmatter 懒加载 + activate_skill 激活）
//
//	go run . -stage 4
//	> 读一下 temp/big.txt                     # 观察 [截断] 与逃生通道提示
//	> 激活 demo 技能，然后做个自我介绍          # activate_skill → 技能约束生效
//	/system                                   # 打印当前轮组装出的系统提示词分段
//	/facts                                    # 打印审计通过的已验证事实
//
// 先造个大文件再玩截断：head -c 20000 /dev/urandom | base64 > temp/big.txt
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/agent/driver/react"
	"github.com/puras/mos/assistant"
	"github.com/puras/mos/llm"
)

const (
	skillsDir  = "skills"
	escapeDir  = "data/escape"
	truncBytes = 2048
	truncLines = 40
	keepRecent = 6
)

func runStage4(model llm.ChatModel, protocol string) *harness {
	rt, reg := baseRunContext(model)
	rt.Observer = printObserver()

	// 槽位式组装器：约束槽固定 + 项目指令（CLAUDE.md 向上递归）+ 技能清单。
	rt.Assembler = agent.NewDefaultSystemAssembler(agent.SystemAssemblerConfig{
		Constraints: defaultSystemPrompt,
		SkillsDir:   skillsDir,
	})

	// 截断器：故意调低阈值，读大文件立刻能看到截断 + 逃生通道。
	rt.Truncator = agent.NewDefaultTruncator(agent.TruncatorConfig{
		MaxBytes: truncBytes,
		MaxLines: truncLines,
		Dir:      escapeDir,
	})

	// 压缩器：保留最近 6 条，超出即触发 fresh-context 压缩
	//（事件流里会出现 compaction/summary，多轮长对话可观察）。
	rt.Compactor = agent.NewDefaultCompactor(agent.CompactorConfig{KeepRecent: keepRecent})

	// 技能：扫描 skills/ 下的 SKILL.md，注册 activate_skill 工具
	//（与 assistant.New 的做法一致）。激活后 AllowedTools 白名单由守卫执行。
	skills, err := agent.LoadSkills(skillsDir)
	check(err)
	if len(skills) > 0 {
		check(reg.Register(assistant.ActivateSkill{Skills: skills}))
	}

	h := &harness{
		protocol: protocol,
		rt:       rt,
		sess:     newSession(),
	}
	h.agent = react.New(rt)
	h.extraBanner = append(h.extraBanner,
		fmt.Sprintf("组装器: 槽位式（约束 + 任务/状态/工具签名 + 技能清单 %d 个）", len(skills)),
		fmt.Sprintf("截断器: %dB / %d 行，逃生目录 %s", truncBytes, truncLines, escapeDir),
		fmt.Sprintf("压缩器: 保留最近 %d 条消息", keepRecent),
		"命令: /system 查看组装产物；/facts 查看已验证事实",
	)

	// /system 打印当前轮会组装出的系统提示词分段（每轮动态，TaskState 变了就变）。
	h.on("/system", func(string) {
		if rt.Assembler == nil {
			fmt.Println("（未配置组装器）")
			return
		}
		sections, err := rt.Assembler.Assemble(context.Background(), h.sess, reg.List())
		if err != nil {
			fmt.Fprintf(os.Stderr, "组装失败: %v\n", err)
			return
		}
		fmt.Printf("系统提示词分段（共 %d 段）：\n", len(sections))
		for _, s := range sections {
			fmt.Printf("── %s ──────────────────\n%s\n", s.Title, short(s.Content, 400))
		}
	})

	// /facts 打印审计门验证过的事实（模型自述不算数，只有只读工具输出入库）。
	h.on("/facts", func(string) {
		fmt.Printf("已验证事实（审计门写入，共 %d 条）：\n", len(h.sess.State.Facts))
		for _, f := range h.sess.State.Facts {
			fmt.Printf("  - [%s] %s\n  证据: %s\n", f.Tool, f.Summary, short(f.Evidence, 80))
		}
	})
	return h
}
