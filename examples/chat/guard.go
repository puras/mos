package main

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/puras/mos/agent/policy"
)

// hostPolicy 是**宿主侧**的参数级检查示例（不是框架默认）。
//
// 为什么在示例里而不是框架里：什么算"危险"由宿主决定，不该由框架替宿主决定。
// PI 把这件事交给扩展（tool_call 事件 + block），DSH 干脆放在隔离层
// （沙箱档位 + 提权审批），两边的框架核心都**没有**危险命令表。
// 框架提供的是**决策点**（deny → 守卫，ask → pre-execute 钩子），规则归宿主。
//
// 这里的两条规则都是词法可判的、不装懂：
//  1. 敏感文件不读不写（.env / id_rsa / *.pem）——凭据不该进模型上下文；
//  2. shell 命令里出现递归删除（rm -rf）→ 需人工确认（ask 档）。
//
// 诚实边界：这是**业务规则**，不是安全边界。真正的边界是文件 jail（工具路径）
// 与进程沙箱（超时/资源/环境）；命令文本判定绕得过去，也不该被当成边界。
func hostPolicy() *policy.ParamPolicy {
	return policy.NewParamPolicy("宿主规则",
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
				if hasRecursiveDelete(cmd) {
					return policy.VerdictAsk, "命令包含递归删除（rm -rf 一类），执行前需要人工确认"
				}
			}
			return policy.VerdictAllow, ""
		},
	)
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

// hasRecursiveDelete 粗判命令里是否有递归删除（rm -rf / rm -fr 一类）。
//
// 词法判定，看不穿别名、变量展开、base64 之类——所以它只是**业务规则**，
// 不是安全边界（真正的边界靠 jail 与沙箱）。
func hasRecursiveDelete(cmd string) bool {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if path.Base(f) != "rm" {
			continue
		}
		for _, a := range fields[i+1:] {
			if strings.HasPrefix(a, "-") && strings.ContainsAny(a, "rR") {
				return true
			}
		}
	}
	return false
}
