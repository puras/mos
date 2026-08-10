package cli

import (
	"log"

	"hegq.net/mos/examples/assistant/internal/config"
	"hegq.net/mos/pkg/tool"
	"hegq.net/mos/pkg/tool/builtin"
	"hegq.net/mos/pkg/tool/extension"
)

// allBuiltinTools 是业务侧默认工具集：
// 内置工具包（builtin.Default，随框架分发） + 扩展工具包（extension：shell / web_search）。
func allBuiltinTools() []tool.Tool {
	// web_search 使用配置指定的搜索提供商（MOS_SEARCH_*）；
	// 构建失败（如缺少 API Key）时回退默认 DuckDuckGo，避免应用无法启动。
	sp, err := config.LoadSearchProvider()
	if err != nil {
		log.Printf("web_search: %v，使用默认 DuckDuckGo", err)
		sp = nil
	}
	return append(builtin.Default(),
		extension.WebSearch{Provider: sp},
		extension.Shell{},
	)
}

// loadTools 装配业务侧工具集：按 MOS_TOOLS 按需启用（逗号分隔工具名），
// 空表示启用全部默认工具。
func loadTools() []tool.Tool {
	all := allBuiltinTools()
	want := config.LoadToolNames()
	if len(want) == 0 {
		return all
	}
	enabled := make(map[string]bool, len(want))
	for _, n := range want {
		enabled[n] = true
	}
	out := make([]tool.Tool, 0, len(want))
	for _, t := range all {
		if enabled[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}
