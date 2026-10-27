package main

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/puras/mos/agent/policy"
)

// hostRules 是**宿主侧**的参数级检查示例（不是框架默认）。
//
// 为什么在示例里而不是框架里：什么算"危险"由宿主决定，不该由框架替宿主决定。
// PI 把这件事交给扩展（tool_call 事件 + block），DSH 干脆放在隔离层
// （沙箱档位 + 提权审批），两边的框架核心都**没有**危险命令表。
// 框架提供的是**决策点**（deny → 守卫，ask → pre-execute 钩子），规则归宿主。
//
// 规则以 []policy.Check 交给装配层：deny 档进守卫（单调拒绝），ask 档转人工
// 确认——框架提供决策点，"什么算危险"由宿主定。
//
// 这里的两条规则都是词法可判的、不装懂：
//  1. 敏感文件不读不写（.env / id_rsa / *.pem）——凭据不该进模型上下文；
//  2. shell 命令里出现递归删除（rm -rf）→ 需人工确认（ask 档）。
//
// 诚实边界：这是**业务规则**，不是安全边界。真正的边界是文件 jail（工具路径）
// 与进程沙箱（超时/资源/环境）；命令文本判定绕得过去，也不该被当成边界。
func hostRules() []policy.Check {
	return []policy.Check{
		func(ctx context.Context, call policy.Call) (policy.Verdict, string) {
			switch call.Tool {
			case "read_file", "write_file", "edit_file":
				p, err := policy.Arg(call, "path")
				if err != nil {
					// 参数读不懂 → 拒绝（fail-closed）。
					return policy.VerdictDeny, "参数解析失败：" + err.Error()
				}
				if sensitivePath(p) {
					return policy.VerdictDeny, fmt.Sprintf("拒绝访问敏感文件 %q（凭据不该进模型上下文）", p)
				}
			case "shell":
				cmd, err := policy.Arg(call, "command")
				if err != nil {
					return policy.VerdictDeny, "参数解析失败：" + err.Error()
				}
				if hasDeleteCommand(cmd) {
					return policy.VerdictAsk, "命令会删除文件，执行前需要人工确认"
				}
			}
			return policy.VerdictAllow, ""
		},
	}
}

// sensitivePath 判断路径是否像敏感文件（凭据/私钥）。
func sensitivePath(p string) bool {
	base := path.Base(strings.ReplaceAll(p, "\\", "/"))
	switch base {
	case ".env", "id_rsa", "id_ed25519", "credentials", ".npmrc", ".netrc":
		return true
	}
	return strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key")
}

// hasDeleteCommand 判断命令是否在**删除东西**。
//
// 键的是「删除命令」本身，不是某个 flag：曾经键的是「含 r 的 flag」——结果
// `git status --short` 里的 r 误触发拦截，而逐个 `git rm`（无 flag）反而漏过，
// 被拒后模型拆解命令绕过了拦截（2026-10 事故）。
//
// 覆盖：rm / unlink / git rm（无论带什么 flag）。rmdir 豁免（只能删空目录，
// 风险低）。
//
// 诚实边界：这只是**业务规则**，不是边界——find -delete、脚本语言、别名、
// 变量拼接都绕得过去。真正的边界是路径强制隔离（S5f-2b，未落地）；在那之前，
// 这条规则的作用是把最常见的删除动作送进人工审批，减少事故面。
func hasDeleteCommand(cmd string) bool {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		switch path.Base(f) {
		case "rm", "unlink":
			return true
		case "git":
			// git rm ...（git 的删除子命令）
			if i+1 < len(fields) && fields[i+1] == "rm" {
				return true
			}
		}
	}
	return false
}
