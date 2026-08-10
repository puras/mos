package longterm

import (
	"fmt"
	"strings"
	"unicode"
)

// ChunkerConfig 是分块策略的配置（RAG 摄取管道）。
type ChunkerConfig struct {
	// ChunkSize 目标分块字节数（默认 800）。
	ChunkSize int
	// Overlap 相邻分块的重叠字节数（默认 100，保留上下文边界）。
	Overlap int
}

// DefaultChunkSize / DefaultOverlap 是默认分块参数。
const (
	DefaultChunkSize = 800
	DefaultOverlap   = 100
)

// Chunker 将长文档切分为有重叠的知识分块（RAG 摄取）。
type Chunker struct {
	chunkSize int
	overlap   int
}

// NewChunker 创建分块器；非正参数回退默认值。
func NewChunker(cfg ChunkerConfig) *Chunker {
	c := &Chunker{chunkSize: cfg.ChunkSize, overlap: cfg.Overlap}
	if c.chunkSize <= 0 {
		c.chunkSize = DefaultChunkSize
	}
	if c.overlap < 0 || c.overlap >= c.chunkSize {
		c.overlap = DefaultOverlap
	}
	return c
}

// Chunk 将文档切分为分块；返回至少 1 个分块（空文档返回空列表）。
// 切割优先落在段落边界（\n\n），其次句号/换行；不足 ChunkSize 则按字节硬切。
func (c *Chunker) Chunk(docID, content string) []Chunk {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if len([]rune(content)) <= c.chunkSize {
		return []Chunk{{DocID: docID, Source: docID, Content: content}}
	}

	var out []Chunk
	runes := []rune(content)
	pos := 0
	idx := 0
	for pos < len(runes) {
		end := pos + c.chunkSize
		cut := -1
		if end >= len(runes) {
			end = len(runes)
		} else {
			// 优先在 chunk 末尾附近的段落/句子边界切割
			if b := boundaryBefore(runes, pos, end, pos+c.chunkSize/2); b > pos {
				end = b
				cut = b
			}
		}
		piece := strings.TrimSpace(string(runes[pos:end]))
		if piece != "" {
			out = append(out, Chunk{
				DocID:   docID,
				Source:  fmt.Sprintf("%s#%d", docID, idx+1),
				Content: piece,
			})
			idx++
		}
		if end == len(runes) {
			break
		}
		if cut > pos {
			// 自然边界（段落/句末）：新块从边界后开始，不 overlap 回溯，保证段落完整
			pos = cut
		} else {
			// 硬切：下一块起点 = 末尾 - 重叠；保证至少前进一个字符（尾部 overlap 吞掉步长时）
			next := end - c.overlap
			if next <= pos {
				next = pos + 1
			}
			pos = next
		}
	}
	return out
}

// boundaryBefore 在 [from, to) 内找最靠后的自然边界（段落/句号/换行），返回边界位置。
// minBound 是允许的最靠前边界（保证分块不会过小）。
func boundaryBefore(runes []rune, from, to, minBound int) int {
	if minBound < from {
		minBound = from
	}
	// 段落边界（\n\n）优先，其次句末（。！？.!?），再退而求换行
	best := -1
	for i := to - 1; i > minBound; i-- {
		if runes[i] == '\n' && i+1 < to && runes[i+1] == '\n' {
			return i + 1
		}
		if isSentenceEnd(runes[i]) && (i+1 >= to || runes[i+1] == ' ' || runes[i+1] == '\n') {
			best = i + 1
		}
	}
	if best > 0 {
		return best
	}
	for i := to - 1; i > minBound; i-- {
		if runes[i] == '\n' {
			return i + 1
		}
	}
	return -1
}

// isSentenceEnd 判断字符是否为中文/英文句末标点。
func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '.', '!', '?', '；', ';':
		return true
	}
	return unicode.IsPunct(r) && (r == '。' || r == '！' || r == '？')
}
