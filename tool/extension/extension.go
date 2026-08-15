// Package extension 提供随框架分发的扩展工具集（Extension Tools）。
//
// 与内置工具包（tool/builtin）不同，扩展工具需要外部服务/提供商注入才能工作：
//   - web_search：联网搜索，依赖 search.Provider 搜索提供商（tool/extension/search）；
//     为空时使用默认 DuckDuckGo。
//
// "依赖注入"的语义与安全策略（fail-closed 的 Allow 名单）天然配合：
// 未显式装配的扩展工具不会进入 LLM 的 function calling 清单。
package extension
