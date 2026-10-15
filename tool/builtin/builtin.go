// Package builtin 提供随框架分发的内置工具集。
//
// 分层约定：框架核心（tool）只提供工具接口、注册表与 Schema 生成，不预设具体工具。
// 本包收录纯标准库即可实现、且无安全副作用的通用工具，按需逐个扩入：
// 涉及文件与命令执行的工具等 filesystem / sandbox 就位后再加入（S5），
// 不先发一版无防护的实现。
package builtin

import "github.com/puras/mos/tool"

// Default 返回内置工具集。
//
// 调用方仍显式决定挂载哪些（显式优于隐式）：可整体装配，也可按 Name 取子集；
// 未装配的工具不进入模型的 function calling 清单。
func Default() []tool.Tool {
	return []tool.Tool{
		CurrentTime{},
	}
}
