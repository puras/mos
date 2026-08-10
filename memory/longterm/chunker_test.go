package longterm

import (
	"strings"
	"testing"
)

func TestChunkerShortDoc(t *testing.T) {
	c := NewChunker(ChunkerConfig{ChunkSize: 800, Overlap: 100})
	chunks := c.Chunk("doc", "短文档不切分")
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if chunks[0].Source != "doc" {
		t.Fatalf("short doc source should be plain docID: %q", chunks[0].Source)
	}
}

func TestChunkerSplits(t *testing.T) {
	c := NewChunker(ChunkerConfig{ChunkSize: 50, Overlap: 10})
	content := strings.Repeat("句子结束。", 20) // 5 字/句 × 20 = 100 字
	chunks := c.Chunk("doc", content)

	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}
	// 每个分块不超过 chunkSize + 一个句子的余量（边界切割允许少量超界）
	for i, ch := range chunks {
		if len([]rune(ch.Content)) > 70 {
			t.Fatalf("chunk %d too big: %d runes: %q", i, len([]rune(ch.Content)), ch.Content)
		}
		if !strings.HasPrefix(ch.Source, "doc#") {
			t.Fatalf("chunk %d source = %q, want doc#N", i, ch.Source)
		}
	}
}

func TestChunkerParagraphBoundary(t *testing.T) {
	c := NewChunker(ChunkerConfig{ChunkSize: 120, Overlap: 10})
	// 首段极长，第二段开头含可检索关键词；分块不应把段落硬切断开
	content := strings.Repeat("甲", 200) + "\n\n" + strings.Repeat("乙", 30)
	chunks := c.Chunk("doc", content)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(chunks))
	}
	// 段首（乙...）应整体保留在一个分块内
	last := chunks[len(chunks)-1]
	if !strings.HasPrefix(last.Content, "乙") {
		t.Fatalf("last chunk = %q, want start with 乙", last.Content)
	}
}

func TestChunkerEmpty(t *testing.T) {
	c := NewChunker(ChunkerConfig{})
	if chunks := c.Chunk("doc", "   "); len(chunks) != 0 {
		t.Fatalf("empty doc should yield no chunks: %+v", chunks)
	}
	if chunks := c.Chunk("doc", ""); len(chunks) != 0 {
		t.Fatalf("empty doc should yield no chunks: %+v", chunks)
	}
}
