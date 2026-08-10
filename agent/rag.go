package agent

import (
	"context"
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

// WithRetriever 挂载长期记忆检索器（M2.7 RAG 注入）：
// 每轮模型调用前按任务目标检索相关知识，经知识守门（源验证 / 冲突裁决）后
// 以 CustomMsg（来源标注 [rag]）注入上下文，携带来源引用供模型核对。
// 未设置时不注入，行为与既往一致。
func WithRetriever(r memory.Retriever, cfg RetrieverConfig) Option {
	return func(a *Agent) {
		if cfg.MaxChunks <= 0 {
			cfg.MaxChunks = 3
		}
		if cfg.Limit <= 0 {
			cfg.Limit = 8
		}
		a.retriever = r
		a.retrieverCfg = cfg
	}
}

// retrieveAndInject 执行 RAG 检索 + 知识守门，返回待发送给模型的注入消息列表。
// 返回的扩展消息不写入会话历史（每轮按最新知识重新检索）。
func (a *Agent) retrieveAndInject(ctx context.Context, sess *Session, turn int, query string) ([]InternalMessage, error) {
	docs, err := a.retriever.Retrieve(ctx, query, a.retrieverCfg.Limit)
	if err != nil {
		return nil, fmt.Errorf("agent: retrieve: %w", err)
	}
	if len(docs) == 0 {
		return nil, nil
	}

	// 冲突裁决：同一来源只保留最高分分块，避免重复注入同源内容
	bySource := make(map[string]memory.Document, len(docs))
	for _, d := range docs {
		key := d.Source
		if key == "" {
			key = d.ID
		}
		cur, ok := bySource[key]
		if !ok || d.Score > cur.Score {
			bySource[key] = d
		}
	}
	selected := make([]memory.Document, 0, len(bySource))
	for _, d := range bySource {
		selected = append(selected, d)
	}
	sortByScore(selected)

	// 知识守门（Agentic RAG）：低于最低分的分块不注入
	passed := selected[:0]
	for _, d := range selected {
		if a.retrieverCfg.MinScore > 0 && d.Score < a.retrieverCfg.MinScore {
			continue
		}
		passed = append(passed, d)
	}
	// 有候选但全部低于阈值 → 知识差距：广播事件，交由模型经外部工具补齐
	if len(passed) == 0 && a.retrieverCfg.MinScore > 0 {
		a.notify(Event{Type: EventKnowledgeGap, SessionID: sess.ID, Turn: turn, Content: query})
		return nil, nil
	}
	if len(passed) > a.retrieverCfg.MaxChunks {
		passed = passed[:a.retrieverCfg.MaxChunks]
	}

	msg := CustomMsg("rag", renderRetrieved(passed))
	return []InternalMessage{msg}, nil
}

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
