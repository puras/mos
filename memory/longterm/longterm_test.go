package longterm

import (
	"context"
	"strings"
	"testing"

	"hegq.net/mos/memory"
)

func openTestStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	s, err := New(":memory:", cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBM25Retrieve(t *testing.T) {
	s := openTestStore(t, Config{})
	ctx := context.Background()

	for _, c := range []Chunk{
		{DocID: "doc-a", Content: "Go 语言并发模型基于 goroutine 与 channel。"},
		{DocID: "doc-b", Content: "SQLite 是嵌入式关系型数据库，支持全文检索。"},
		{DocID: "doc-c", Content: "今天天气晴朗，适合户外运动。"},
	} {
		if err := s.AddChunk(ctx, c); err != nil {
			t.Fatalf("AddChunk: %v", err)
		}
	}

	docs, err := s.Retrieve(ctx, "Go 并发", 3)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("no results")
	}
	// BM25 命中关键词的分块应排第一
	if !strings.Contains(docs[0].Content, "Go") {
		t.Fatalf("docs[0] = %+v, want Go-related top", docs[0])
	}
	if docs[0].Source != "doc-a" {
		t.Fatalf("docs[0].Source = %q", docs[0].Source)
	}
}

func TestRetrieveLimitAndEmpty(t *testing.T) {
	s := openTestStore(t, Config{})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.AddChunk(ctx, Chunk{DocID: "d", Content: "Go 语言版本 " + strings.Repeat("x", i)}); err != nil {
			t.Fatalf("AddChunk: %v", err)
		}
	}
	docs, err := s.Retrieve(ctx, "Go 语言", 2)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs = %d, want 2", len(docs))
	}
	// 空查询：无匹配，返回空（不报错）
	if docs, err := s.Retrieve(ctx, "   ", 3); err != nil || len(docs) != 0 {
		t.Fatalf("empty query: docs=%v err=%v, want empty", docs, err)
	}
}

func TestDeleteDoc(t *testing.T) {
	s := openTestStore(t, Config{})
	ctx := context.Background()
	if err := s.AddChunk(ctx, Chunk{DocID: "doc-a", Content: "Go 并发"}); err != nil {
		t.Fatalf("AddChunk: %v", err)
	}
	if err := s.DeleteDoc(ctx, "doc-a"); err != nil {
		t.Fatalf("DeleteDoc: %v", err)
	}
	docs, err := s.Retrieve(ctx, "Go 并发", 3)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("docs = %+v, want empty after delete", docs)
	}
}

func TestAddDocumentWithChunker(t *testing.T) {
	s := openTestStore(t, Config{})
	ctx := context.Background()
	chunker := NewChunker(ChunkerConfig{ChunkSize: 60, Overlap: 10})

	doc := strings.Repeat("Go 语言特性介绍。", 10)
	if err := s.AddDocument(ctx, "doc-go", doc, chunker); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}

	docs, err := s.Retrieve(ctx, "Go 语言", 5)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("no results after AddDocument")
	}
	// 来源引用：doc-go#N（分块序号）
	if !strings.HasPrefix(docs[0].Source, "doc-go#") {
		t.Fatalf("source = %q, want doc-go#N", docs[0].Source)
	}
}

// hashEmbedder 确定性伪向量：按关键词命中编码 2 维向量（测试用）。
type hashEmbedder struct{}

func (hashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, 2)
	if strings.Contains(text, "并发") {
		v[0] = 1
	}
	if strings.Contains(text, "数据库") {
		v[1] = 1
	}
	return v, nil
}

func TestHybridRetrieve(t *testing.T) {
	s := openTestStore(t, Config{Embedder: hashEmbedder{}, Dim: 2})
	ctx := context.Background()

	for _, c := range []Chunk{
		{DocID: "doc-1", Content: "Go 并发模型：goroutine 与 channel。"},
		{DocID: "doc-2", Content: "SQLite 数据库支持事务。"},
		{DocID: "doc-3", Content: "并发编程中数据库连接池很重要。"},
	} {
		if err := s.AddChunk(ctx, c); err != nil {
			t.Fatalf("AddChunk: %v", err)
		}
	}

	// 查询"并发 数据库"：doc-3 同时命中两个语义维度的向量，应排第一
	docs, err := s.Retrieve(ctx, "并发 数据库", 3)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("no results")
	}
	if docs[0].Source != "doc-3" {
		t.Fatalf("docs[0] = %+v, want doc-3 (hybrid 语义最相关)", docs[0])
	}
	// 融合得分应在 (0,1] 区间（BM25 归一化 + 余弦加权）
	if docs[0].Score <= 0 || docs[0].Score > 1 {
		t.Fatalf("score = %f, want in (0,1]", docs[0].Score)
	}
}

// longterm.Store 实现 memory.Retriever（编译期断言）。
var _ memory.Retriever = (*Store)(nil)
