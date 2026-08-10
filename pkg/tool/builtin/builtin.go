// Package builtin 提供随框架分发的内置工具集（Builtin Tools）。
//
// 分层约定：框架核心（pkg/tool）只提供工具接口、注册表与 Schema 生成，
// 不预设任何工具；本包是框架内置的标准工具集，覆盖纯标准库即可实现的通用能力
// （计算 / 时间 / 文件读写检索 / 网页抓取），随框架一起分发，外部工程可直接复用。
//
// 需要依赖注入的工具（shell 沙箱执行、web_search 搜索提供商）归入
// pkg/tool/extension 扩展工具包，与本包 Default() 拼接装配。
package builtin

import "hegq.net/mos/pkg/tool"

// Default 返回内置工具集的完整列表：calculator、current_time、read_file、
// write_file、find_files、grep、web_fetch。
//
// 调用方仍显式决定挂载哪些工具（显式优于隐式）：可直接整体装配，
// 也可按 Name 过滤子集；未装配的工具不进入 LLM 的 function calling 清单。
func Default() []tool.Tool {
	return []tool.Tool{
		Calculator{},
		CurrentTime{},
		ReadFile{},
		WriteFile{},
		FindFiles{},
		Grep{},
		WebFetch{},
	}
}
