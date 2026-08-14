package agent

import (
	"fmt"
	"strings"

	"hegq.net/mos/memory"
)

// RetrieverConfig 是 RAG 检索注入（M2.7）的配置。
type RetrieverConfig struct {
	// MaxChunks 每次注入上下文的最大分块数（默认 3）。
	// 经冲突裁决（同来源只保留最高分分块）后按得分取前 MaxChunks。
	MaxChunks int
	// Limit 每次检索从后端取回的分块数（默认 8，为裁决留出候选）。
	Limit int
	// MinScore 知识守门最低分（Agentic RAG 知识差距检测）：
	// >0 时低于该阈值的分块不注入，全部低于阈值则判定"知识差距"——
	// 广播 EventKnowledgeGap（Content 为查询），交由模型经外部工具补齐。
	// 得分尺度取决于检索后端（hybrid 路径为 [0,1] 融合分）；0 表示不过滤。
	MinScore float64
}

// Retriever 配置经 RunContext.Retriever + RetrieverCfg 注入（v2：见 agent/react.go
// ragInjectHook）；RetrieverConfig 保留为配置结构。

// renderRetrieved 将检索结果渲染为注入文本：逐条标注来源与得分。
func renderRetrieved(docs []memory.Document) string {
	var b strings.Builder
	b.WriteString("检索到的相关记忆：\n")
	for _, d := range docs {
		fmt.Fprintf(&b, "- [来源: %s] (score %.2f): %s\n", d.Source, d.Score, strings.TrimSpace(d.Content))
	}
	return strings.TrimSpace(b.String())
}

func sortByScore(docs []memory.Document) {
	for i := 1; i < len(docs); i++ {
		for j := i; j > 0 && docs[j].Score > docs[j-1].Score; j-- {
			docs[j], docs[j-1] = docs[j-1], docs[j]
		}
	}
}
