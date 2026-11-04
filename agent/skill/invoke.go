package skill

import "strings"

// ExpandInvocation 展开用户文本里的技能手势（DSH 的 /name 同款）。
//
// 手势 = 文本的**首个** token 形如 /name，且 name 是已知技能：命中就把手势
// 换成技能正文块，其后的文字（参数）追加在正文之后（PI 同款：args 追加正文
// 之后）。
//
// 框架**不自动扫**用户文本：误触发风险（用户只是在句子里提到 "/deploy" 就
// 展开一大段正文），而且 SDK 没有用户命令面。由宿主决定何时调用（拿到
// 用户输入时调一次即可）。
//
// 未命中返回原文与 false——那不是错误（那句话可能根本不是手势）。
//
// 仅用户可调用的技能（disable-model-invocation）在这里**可以**展开：这正是
// 该标记的用途（模型不许调，用户可以）。
func (c *Catalog) ExpandInvocation(text string) (string, bool) {
	if c == nil {
		return text, false
	}
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(trimmed, "/") {
		return text, false
	}
	rest := trimmed[1:]
	name := rest
	tail := ""
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		name = rest[:i]
		tail = strings.TrimSpace(rest[i+1:])
	}
	sk, ok := c.Lookup(name)
	if !ok {
		return text, false
	}
	out := sk.RenderContent()
	if tail != "" {
		out += "\n\n" + tail
	}
	return out, true
}
