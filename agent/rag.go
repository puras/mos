package agent

// RetrieverConfig 是 RAG 检索注入的配置。
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

// Retriever 配置经 RunContext.Retriever + RetrieverCfg 注入（见 react 包
// ragInjectHook）；未注入 Retriever 时跳过检索注入，行为不变。
