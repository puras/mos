// Package longterm 提供基于 SQLite 的长期记忆（RAG 基础设施，M2.7）。
//
// 存储层使用 modernc.org/sqlite（纯 Go，无 CGO），BM25 关键词检索走 FTS5 全文索引；
// 向量检索在 Go 侧计算余弦相似度（sqlite-vec 扩展在纯 Go 驱动下不可可靠加载，
// 语义等价：混合检索 = BM25 得分 + 向量相似度加权融合）。
// 未配置 Embedder 时退化为纯 BM25 关键词检索。
package longterm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode"

	_ "modernc.org/sqlite"

	"hegq.net/mos/memory"
)

// Embedder 将文本编码为向量（由应用层注入，如 OpenAI embeddings）。
// 返回 nil 表示该实现不支持向量；长文本建议先分块再逐块编码。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Chunk 是待写入长期记忆的一个知识分块。
type Chunk struct {
	// DocID 是来源文档标识（文件路径 / URL / 会话 ID 等）。
	DocID string
	// Source 是来源引用（默认取 DocID；可细化到文件+偏移或 URL 片段）。
	Source string
	// Content 是分块内容。
	Content string
	// Embedding 是可选的内容向量（由调用方或 Store 内部经 Embedder 计算）。
	Embedding []float32
}

// Config 是 Store 的配置。
type Config struct {
	// Embedder 可选的向量编码器；nil 时检索退化为纯 BM25 关键词。
	Embedder Embedder
	// Dim 向量维度（配置了 Embedder 时须与 Embedder 输出一致）。
	Dim int
	// BM25Weight 与 VectorWeight 混合检索权重（默认 0.7 / 0.3）。
	BM25Weight float64
	VectorWeight float64
	// TopK 混合检索向量阶段保留的候选数（默认 64）。
	TopK int
}

// Store 是 SQLite 支撑的长期记忆检索器，实现 memory.Retriever。
type Store struct {
	db   *sql.DB
	cfg  Config
}

// New 打开（或创建）SQLite 数据库文件并初始化表结构。
// path 为 ":memory:" 时使用内存库（测试/临时场景）。
func New(path string, cfg Config) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("longterm: open %s: %w", path, err)
	}
	if cfg.BM25Weight == 0 && cfg.VectorWeight == 0 {
		cfg.BM25Weight, cfg.VectorWeight = 0.7, 0.3
	}
	if cfg.TopK <= 0 {
		cfg.TopK = 64
	}
	s := &Store{db: db, cfg: cfg}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// init 建表：chunks 主表（embedding 存 JSON 文本列，维度规模下 Go 侧余弦足够）+ FTS5 全文索引。
// search_text 列是 CJK 逐字空格化后的检索文本（unicode61 tokenizer 无法直接切分无空格中文）；
// chunks_fts 只索引 search_text，命中后 JOIN chunks 取原始 content 展示。
func (s *Store) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS chunks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			doc_id TEXT NOT NULL,
			source TEXT NOT NULL,
			content TEXT NOT NULL,
			search_text TEXT NOT NULL DEFAULT '',
			embedding TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(doc_id)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(search_text)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("longterm: init schema: %w", err)
		}
	}
	// 触发器：chunks 写入/删除时同步 FTS 索引。
	// 注意：modernc SQLite 的 FTS5 'delete' 命令（INSERT INTO fts(fts,rowid,col) VALUES('delete',...)）
	// 不可靠（报 SQL logic error），改用 DELETE 语句按 rowid 删除索引项。
	// 先 DROP 再 CREATE：保证旧触发器定义（如 'delete' 命令版本）被最新定义覆盖。
	triggers := []string{
		`DROP TRIGGER IF EXISTS chunks_ai`,
		`CREATE TRIGGER chunks_ai AFTER INSERT ON chunks BEGIN
			INSERT INTO chunks_fts(rowid, search_text) VALUES (new.id, new.search_text);
		END`,
		`DROP TRIGGER IF EXISTS chunks_ad`,
		`CREATE TRIGGER chunks_ad AFTER DELETE ON chunks BEGIN
			DELETE FROM chunks_fts WHERE rowid = old.id;
		END`,
	}
	for _, q := range triggers {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("longterm: init triggers: %w", err)
		}
	}
	return nil
}

// AddChunk 写入一个知识分块（自动计算向量并维护 FTS 索引）。
func (s *Store) AddChunk(ctx context.Context, c Chunk) error {
	source := c.Source
	if source == "" {
		source = c.DocID
	}
	emb := c.Embedding
	if emb == nil && s.cfg.Embedder != nil {
		v, err := s.cfg.Embedder.Embed(ctx, c.Content)
		if err != nil {
			return fmt.Errorf("longterm: embed: %w", err)
		}
		emb = v
	}
	var embJSON string
	if emb != nil {
		b, err := json.Marshal(emb)
		if err != nil {
			return fmt.Errorf("longterm: marshal embedding: %w", err)
		}
		embJSON = string(b)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chunks(doc_id, source, content, search_text, embedding) VALUES (?, ?, ?, ?, ?)`,
		c.DocID, source, c.Content, cjkSpace(c.Content), embJSON)
	if err != nil {
		return fmt.Errorf("longterm: add chunk: %w", err)
	}
	return nil
}

// AddDocument 将一份文档按 chunker 分块后批量写入（同一 DocID 先删旧再写，实现文档级更新）。
func (s *Store) AddDocument(ctx context.Context, docID, content string, chunker *Chunker) error {
	if err := s.DeleteDoc(ctx, docID); err != nil {
		return err
	}
	for _, c := range chunker.Chunk(docID, content) {
		if err := s.AddChunk(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// DeleteDoc 删除某来源文档的全部分块。
func (s *Store) DeleteDoc(ctx context.Context, docID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chunks WHERE doc_id = ?`, docID); err != nil {
		return fmt.Errorf("longterm: delete doc: %w", err)
	}
	return nil
}

// Retrieve 实现 memory.Retriever：BM25（FTS5）+ 向量余弦（可选）混合检索。
// 未配置 Embedder 时退化为纯 BM25。
func (s *Store) Retrieve(ctx context.Context, query string, limit int) ([]memory.Document, error) {
	if limit <= 0 {
		return nil, nil
	}
	if s.cfg.Embedder != nil {
		v, err := s.cfg.Embedder.Embed(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("longterm: embed query: %w", err)
		}
		return s.hybridRetrieve(ctx, query, v, limit)
	}
	return s.bm25Retrieve(ctx, query, limit)
}

// bm25Retrieve 纯 BM25 检索：FTS5 MATCH + bm25() 得分（负分，越小越相关）。
func (s *Store) bm25Retrieve(ctx context.Context, query string, limit int) ([]memory.Document, error) {
	match, ok := toFTSQuery(query)
	if !ok {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.source, c.content, bm25(chunks_fts) AS score
		FROM chunks_fts JOIN chunks c ON c.id = chunks_fts.rowid
		WHERE chunks_fts MATCH ?
		ORDER BY score LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("longterm: bm25 search: %w", err)
	}
	defer rows.Close()
	var out []memory.Document
	for rows.Next() {
		var id int64
		var src, content string
		var bm float64
		if err := rows.Scan(&id, &src, &content, &bm); err != nil {
			return nil, err
		}
		out = append(out, memory.Document{ID: fmt.Sprint(id), Source: src, Content: content, Score: -bm})
	}
	return out, rows.Err()
}

// hybridRetrieve 混合检索：先 BM25 取 TopK 候选，再叠加向量余弦相似度加权融合。
func (s *Store) hybridRetrieve(ctx context.Context, query string, qvec []float32, limit int) ([]memory.Document, error) {
	match, ok := toFTSQuery(query)
	if !ok {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.source, c.content, c.embedding, bm25(chunks_fts) AS bm
		FROM chunks_fts JOIN chunks c ON c.id = chunks_fts.rowid
		WHERE chunks_fts MATCH ?
		ORDER BY bm LIMIT ?`, match, s.cfg.TopK)
	if err != nil {
		return nil, fmt.Errorf("longterm: hybrid search: %w", err)
	}
	defer rows.Close()

	type cand struct {
		id  int64
		src string
		con string
		bm  float64
		vec []float32
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var embJSON sql.NullString
		if err := rows.Scan(&c.id, &c.src, &c.con, &embJSON, &c.bm); err != nil {
			return nil, err
		}
		if embJSON.Valid && embJSON.String != "" {
			_ = json.Unmarshal([]byte(embJSON.String), &c.vec)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// BM25 得分归一化到 [0,1]（bm 为负分：-bm 越大越相关）
	maxBm := 1.0
	for _, c := range cands {
		if v := -c.bm; v > maxBm {
			maxBm = v
		}
	}
	wB, wV := s.cfg.BM25Weight, s.cfg.VectorWeight
	out := make([]memory.Document, 0, len(cands))
	for _, c := range cands {
		score := wB * (-c.bm / maxBm)
		if len(c.vec) > 0 && len(qvec) > 0 {
			score += wV * cosine(c.vec, qvec)
		}
		out = append(out, memory.Document{ID: fmt.Sprint(c.id), Source: c.src, Content: c.con, Score: score})
	}
	// 按融合得分降序，截断到 limit
	sortDocs(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// toFTSQuery 将用户查询转换为 FTS5 MATCH 表达式：
// 每个词先做 CJK 空格化（与存储的 search_text 同规则），再加引号（避免 FTS5 语法字符），
// 用空格连接（默认 AND 语义）。
func toFTSQuery(q string) (string, bool) {
	terms := strings.Fields(q)
	var parts []string
	for _, t := range terms {
		parts = append(parts, `"`+strings.ReplaceAll(cjkSpace(t), `"`, `""`)+`"`)
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, " "), true
}

// cjkSpace 在连续 CJK 字符之间插入空格：unicode61 tokenizer 按空白/标点分词，
// 无空格的中文整句会被当成一个 token 导致关键词无法命中；逐字空格化后每个汉字成为独立 token，
// 查询词同样空格化并作为 phrase（如 "并 发"）匹配，实现中文关键词检索。
func cjkSpace(s string) string {
	var b strings.Builder
	prev := false
	for _, r := range s {
		cjk := unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
		if cjk && prev {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		prev = cjk
	}
	return b.String()
}

// cosine 计算两个向量的余弦相似度（向量为空时返回 0）。
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func sortDocs(docs []memory.Document) {
	for i := 1; i < len(docs); i++ {
		for j := i; j > 0 && docs[j].Score > docs[j-1].Score; j-- {
			docs[j], docs[j-1] = docs[j-1], docs[j]
		}
	}
}
