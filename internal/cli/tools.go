package cli

import (
	"log"

	"hegq.net/mos/internal/config"
	"hegq.net/mos/internal/tool/builtin"
	"hegq.net/mos/pkg/tool"
)

// allBuiltinTools 是业务侧默认工具集：框架不预设工具，由应用层显式装配。
func allBuiltinTools() []tool.Tool {
	// web_search 使用配置指定的搜索提供商（MOS_SEARCH_*）；
	// 构建失败（如缺少 API Key）时回退默认 DuckDuckGo，避免应用无法启动。
	sp, err := config.LoadSearchProvider()
	if err != nil {
		log.Printf("web_search: %v，使用默认 DuckDuckGo", err)
		sp = nil
	}
	return []tool.Tool{
		builtin.Calculator{},
		builtin.CurrentTime{},
		builtin.WebFetch{},
		builtin.WebSearch{Provider: sp},
		builtin.Shell{},
		builtin.ReadFile{},
		builtin.WriteFile{},
		builtin.FindFiles{},
		builtin.Grep{},
	}
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
