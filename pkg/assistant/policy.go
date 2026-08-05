package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"hegq.net/mos/pkg/llm"
)

// DefaultPolicy 是默认的工具调用安全策略（Harness Engineering）：
//   - shell：基础危险命令防御 + 可选命令前缀白名单
//   - 文件类工具：路径越界防护，限制在 WorkDir 内
//   - web_fetch：仅允许 http/https（工具内已校验）
type DefaultPolicy struct {
	ShellAllowed []string // 命令前缀白名单；空表示仅基础防御
	WorkDir      string   // 文件工具根目录；空表示不限制
}

// NewDefaultPolicy 创建默认安全策略。
func NewDefaultPolicy(shellAllowed []string, workDir string) *DefaultPolicy {
	return &DefaultPolicy{ShellAllowed: shellAllowed, WorkDir: workDir}
}

// Allow 实现 agent.Policy。
func (p *DefaultPolicy) Allow(_ context.Context, call *llm.ToolCall) error {
	switch call.Name {
	case "shell":
		return p.checkShell(call.Args)
	case "read_file", "write_file":
		return p.checkFile(call.Args, "path")
	case "find_files", "grep":
		return p.checkFile(call.Args, "root")
	case "web_fetch":
		var args struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(call.Args), &args) == nil && args.URL != "" {
			if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
				return fmt.Errorf("仅允许抓取 http/https 地址")
			}
		}
	}
	return nil
}

// checkShell 校验 shell 命令：危险命令防御 + 白名单。
func (p *DefaultPolicy) checkShell(argsJSON string) error {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Errorf("无法解析命令参数: %w", err)
	}
	cmd := strings.TrimSpace(args.Command)
	if cmd == "" {
		return fmt.Errorf("命令不能为空")
	}
	if bad := dangerousCommand(cmd); bad != "" {
		return fmt.Errorf("命令包含危险操作 %q，已拒绝", bad)
	}
	if len(p.ShellAllowed) > 0 && !matchPrefixes(cmd, p.ShellAllowed) {
		return fmt.Errorf("命令不在允许列表，允许前缀: %s", strings.Join(p.ShellAllowed, ", "))
	}
	return nil
}

// checkFile 校验文件路径不超出工作目录。
func (p *DefaultPolicy) checkFile(argsJSON, field string) error {
	if p.WorkDir == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Errorf("无法解析参数: %w", err)
	}
	raw, ok := args[field].(string)
	if !ok || raw == "" {
		return nil
	}
	root, err := filepath.Abs(p.WorkDir)
	if err != nil {
		return err
	}
	target := raw
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %q 超出工作目录 %q", raw, root)
	}
	return nil
}

// dangerousCommand 检测危险命令模式，返回命中的关键词。
func dangerousCommand(cmd string) string {
	patterns := []string{
		"rm -rf", "mkfs", "dd if", "shutdown", "reboot", "sudo",
		"chown -R", "chmod -R 777", ":(){", "> /dev/sd",
		"| sh", "| bash", "kill -9", "halt", "poweroff",
	}
	for _, p := range patterns {
		if strings.Contains(cmd, p) {
			return p
		}
	}
	return ""
}

// matchPrefixes 判断命令是否以白名单中任意前缀开头。
func matchPrefixes(cmd string, prefixes []string) bool {
	for _, p := range prefixes {
		if cmd == p || strings.HasPrefix(cmd, p+" ") || strings.HasPrefix(cmd, p+"\t") {
			return true
		}
	}
	return false
}
