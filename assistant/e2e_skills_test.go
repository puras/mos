package assistant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hegq.net/mos/agent/event"
	"hegq.net/mos/llm"
	"hegq.net/mos/tool"
	"hegq.net/mos/tool/builtin"
)

// writeSKILL 写测试用 SKILL.md（frontmatter + body）。
func writeSKILL(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// hasToolResult 报告消息历史中是否已出现工具结果（用于脚本模型判断轮次）。
func hasToolResult(msgs []llm.Message) bool {
	for _, m := range msgs {
		if m.Role == llm.RoleTool {
			return true
		}
	}
	return false
}

// activateSkillModel 模拟两轮：第一次 activate_skill，第二次直接作答。
func activateSkillModel() llm.ChatModel { return scriptedSkillModel{} }

type scriptedSkillModel struct{}

func (scriptedSkillModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }

func (scriptedSkillModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	// 第一轮：发起 activate_skill 调用
	if !hasToolResult(req.Messages) {
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "我要激活 git 技能",
			ToolCalls: []llm.ToolCall{{
				ID: "act1", Name: "activate_skill",
				Args: `{"name":"git-helper"}`,
			}},
		}}, nil
	}
	// 第二轮（看到工具结果）：直接作答
	return &llm.Response{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: "已激活 git 技能",
	}}, nil
}

func (scriptedSkillModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// safeReaderBlockedModel 三轮脚本：第一轮 activate_skill(safe-reader)，
// 第二轮尝试 shell（被 allow-list 拦截），第三轮看到拒绝后直接作答。
func safeReaderBlockedModel() llm.ChatModel { return blockedToolModel{} }

type blockedToolModel struct{}

func (blockedToolModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }

func (blockedToolModel) Chat(_ context.Context, req *llm.Request) (*llm.Response, error) {
	toolMsgs := 0
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			toolMsgs++
		}
	}
	switch toolMsgs {
	case 0:
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "激活 safe-reader",
			ToolCalls: []llm.ToolCall{{
				ID: "act2", Name: "activate_skill",
				Args: `{"name":"safe-reader"}`,
			}},
		}}, nil
	case 1:
		// 看到 activate_skill 结果后尝试 shell
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "试试 shell",
			ToolCalls: []llm.ToolCall{{
				ID: "sh1", Name: "shell",
				Args: `{"command":"ls"}`,
			}},
		}}, nil
	default:
		// 看到拒绝后直接作答
		return &llm.Response{Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: "shell 被拦截了",
		}}, nil
	}
}

func (blockedToolModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

// TestE2E_ActivateSkillFlow 端到端：模型首轮调 activate_skill，driver 注入 sess
// 写 skill/activate 事件，次轮模型返回最终回复，轮结束写 skill/deactivate。
// 装配阶段 SystemAssembler 写 skill/load。
func TestE2E_ActivateSkillFlow(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, filepath.Join(dir, "git", "SKILL.md"),
		"---\nname: git-helper\ndescription: git 操作\nallowed-tools:\n  - read_file\n---\n# Git Body\n")

	a, err := New(activateSkillModel(), nil, Config{SkillsDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sess := a.NewSession()
	reply, err := a.Run(context.Background(), sess, "启动 git 任务")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(reply, "已激活 git 技能") {
		t.Errorf("reply = %q, want 包含已激活", reply)
	}

	if !hasEvent(sess.Events, event.SessionEventSkillLoad) {
		t.Errorf("missing skill/load event in: %v", sess.Events)
	}
	if !hasEvent(sess.Events, event.SessionEventSkillActivate) {
		t.Errorf("missing skill/activate event in: %v", sess.Events)
	}
	if !hasEvent(sess.Events, event.SessionEventSkillDeactivate) {
		t.Errorf("missing skill/deactivate event in: %v", sess.Events)
	}
}

// TestE2E_SkillAllowedToolBlocked 端到端：turn 1 激活 safe-reader（仅 read_file）后，
// turn 2 模型尝试 shell 调用被 skillAllowListGuard 拒绝；拒绝原因经 tool/result
// 事件落库，模型 turn 3 看到并收尾。
//
// 这是 allowed-tools 硬限制的**真实生效窗口**：跨轮。激活态作用域为本次 Run
// （见 react.Run 起始处说明），turn 1 写入的 subset 在 turn 2 的 preflight 仍在，
// guard 据此拒绝白名单外的 shell。
//
// 已知不生效窗口（设计固有，非缺陷）：模型在**同一批**里同时发 activate_skill
// 与 shell 时，agent/executor 阶段 1 串行跑完整批 preflight 才进入阶段 2 执行，
// shell 的 preflight 早于 activate_skill 的 Execute，此刻 subset 仍为 nil → 放行。
// 该窗口本身无意义：模型尚未读到技能正文，谈不上"按技能指引调工具"。
func TestE2E_SkillAllowedToolBlocked(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, filepath.Join(dir, "safe", "SKILL.md"),
		"---\nname: safe-reader\ndescription: 仅读\nallowed-tools:\n  - read_file\n---\n# safe\n")

	a, err := New(safeReaderBlockedModel(), []tool.Tool{builtin.Shell{}}, Config{SkillsDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sess := a.NewSession()
	reply, err := a.Run(context.Background(), sess, "执行命令")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 断言 shell 的 tool/result 事件 Error 字段含 skill allow-list 字样
	var blocked bool
	for _, e := range sess.Events {
		if e.Type != event.SessionEventToolResult {
			continue
		}
		var d event.ToolResultData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			continue
		}
		if d.Name == "shell" && strings.Contains(d.Error, "skill allow-list") {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Errorf("expected shell tool/result with skill allow-list denial; reply=%q", reply)
	}
}
