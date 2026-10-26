package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/puras/mos/llm"
)

func mustAppend(t *testing.T, s *Session, ev Event) Event {
	t.Helper()
	out, err := s.Append(ev)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return out
}

func TestAppendSeqContinuityAndCopy(t *testing.T) {
	s := New("t1", nil)
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Text: "你好"})
	ev := mustAppend(t, s, Event{Kind: EventAssistant, Text: "你好，需要什么帮助？"})

	if ev.Seq != 3 {
		t.Fatalf("seq = %d, want 3（全局递增从 1 开始）", ev.Seq)
	}
	if ev.Time.IsZero() {
		t.Fatal("Append 应填充 Time")
	}

	// Events 返回副本：改动副本不影响日志
	got := s.Events()
	got[0].Kind = "hacked"
	if s.Events()[0].Kind == "hacked" {
		t.Fatal("Events 必须返回副本，调用方不得改动日志")
	}
}

func TestProjection(t *testing.T) {
	s := New("t2", nil)
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Text: "看下文件"})
	mustAppend(t, s, Event{Kind: EventAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"a.txt"}`}}})
	// tool/call 仅审计留痕，不投影
	mustAppend(t, s, Event{Kind: EventToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "read_file"}})
	mustAppend(t, s, Event{Kind: EventToolResult, ToolResult: &ToolResult{CallID: "c1", Content: "文件内容"}})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "内容是……"})
	mustAppend(t, s, Event{Kind: EventTurnEnd, Turn: 1, Reason: "completed"})

	msgs := s.Messages()
	want := []struct {
		role    string
		content string
	}{
		{"user", "看下文件"},
		{"assistant", ""}, // 携带 ToolCalls
		{"tool", "文件内容"},
		{"assistant", "内容是……"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("投影 %d 条消息, want %d（turn/* 与 tool/call 不投影）", len(msgs), len(want))
	}
	for i, w := range want {
		if string(msgs[i].Role) != w.role || msgs[i].Content != w.content {
			t.Fatalf("msgs[%d] = %s/%q, want %s/%q", i, msgs[i].Role, msgs[i].Content, w.role, w.content)
		}
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "read_file" {
		t.Fatalf("assistant 消息应携带 ToolCalls: %+v", msgs[1])
	}
	if msgs[2].ToolCallID != "c1" {
		t.Fatalf("tool 消息应携带 ToolCallID: %+v", msgs[2])
	}
}

func TestJSONLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	jl, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	id := "rt"
	if err := jl.SaveHeader(id, Header{ID: id}); err != nil {
		t.Fatalf("SaveHeader: %v", err)
	}

	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Text: "你好"})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "嗨", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}})
	mustAppend(t, s, Event{Kind: EventTurnEnd, Turn: 1, Reason: "completed"})

	events, err := jl.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(events) != 4 || events[0].Kind != EventTurnStart || events[2].Text != "嗨" {
		t.Fatalf("round-trip 不一致: %+v", events)
	}
	if events[0].Seq != 1 || events[3].Seq != 4 {
		t.Fatalf("Seq 未持久化: %d..%d", events[0].Seq, events[3].Seq)
	}

	// Restore 重建：日志与投影一致，且可继续追加
	rs, err := Restore(id, jl)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(rs.Messages()) != 2 {
		t.Fatalf("恢复后投影 = %d 条, want 2", len(rs.Messages()))
	}
	if got := rs.TotalUsage(); got.TotalTokens != 15 {
		t.Fatalf("恢复后 TotalUsage = %+v, want TotalTokens 15（用量随事件重放重建）", got)
	}
	mustAppend(t, rs, Event{Kind: EventUser, Text: "继续"})
	events, _ = jl.Load(id)
	if len(events) != 5 || events[4].Seq != 5 {
		t.Fatalf("恢复后续写应持久化且 Seq 续接: %+v", events)
	}
}

func TestRestoreRepairsOrphanTurn(t *testing.T) {
	dir := t.TempDir()
	jl, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	id := "orphan"
	jl.SaveHeader(id, Header{ID: id})
	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Text: "任务"})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "干一半"}) // 崩溃：无 turn/end

	rs, err := Restore(id, jl)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	events := rs.Events()
	last := events[len(events)-1]
	if last.Kind != EventTurnEnd || last.Reason != "interrupted" {
		t.Fatalf("孤儿 turn 应合成 interrupted 收尾, got %s/%s", last.Kind, last.Reason)
	}
	if last.Seq != 4 {
		t.Fatalf("合成事件 Seq = %d, want 4（内存续接盘上 3 条，不回写）", last.Seq)
	}
	// 不回写已有日志：后端里仍只有 3 条原始事件
	disk, _ := jl.Load(id)
	if len(disk) != 3 {
		t.Fatalf("合成事件不得回写后端: 磁盘 %d 条", len(disk))
	}

	// 完整日志不应误报孤儿
	s2 := New("ok", jl)
	mustAppend(t, s2, Event{Kind: EventTurnStart, Turn: 2})
	mustAppend(t, s2, Event{Kind: EventTurnEnd, Turn: 2, Reason: "completed"})
	if s2.repairOrphanTurn() {
		t.Fatal("尾部完整的日志不应报孤儿 turn")
	}
}

func TestAppendFailFast(t *testing.T) {
	boom := errors.New("disk full")
	s := New("ff", failPersist{err: boom})
	if _, err := s.Append(Event{Kind: EventUser, Text: "x"}); !errors.Is(err, boom) {
		t.Fatalf("写失败应返回错误, got %v", err)
	}
	// fail-fast：写失败的事件不得进内存
	if got := s.Events(); len(got) != 0 {
		t.Fatalf("写失败后日志应为空, got %d 条", len(got))
	}
}

type failPersist struct{ err error }

func (f failPersist) SaveHeader(string, Header) error { return nil }
func (f failPersist) Append(string, Event) error      { return f.err }
func (f failPersist) Load(string) ([]Event, error)    { return nil, nil }
func (f failPersist) Close() error                    { return nil }

func TestJSONLTrailingHalfLine(t *testing.T) {
	dir := t.TempDir()
	jl, _ := NewJSONL(dir)
	id := "half"
	jl.SaveHeader(id, Header{ID: id})
	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventUser, Text: "完整的一条"})

	// 模拟崩溃：追加半行 JSON（无换行结尾）
	p := filepath.Join(dir, id+".jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"seq":2,"kind":"user","te`)
	f.Close()

	events, err := jl.Load(id)
	if err != nil {
		t.Fatalf("尾部半行应被容忍: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("半行应被丢弃, got %d 条", len(events))
	}

	// 中段损坏必须报错，不允许静默跳过
	full, _ := os.ReadFile(p)
	corrupted := strings.Replace(string(full), `"完整的一条"`, `"损坏"`, 1) + "\n" + `{bad json}` + "\n"
	os.WriteFile(p, []byte(corrupted), 0o644)
	if _, err := jl.Load(id); err == nil {
		t.Fatal("中段损坏应返回错误")
	}
}

func TestRestoreMissingSession(t *testing.T) {
	jl, _ := NewJSONL(t.TempDir())
	s, err := Restore("nope", jl)
	if err != nil {
		t.Fatalf("不存在的会话应返回空会话而非错误: %v", err)
	}
	if len(s.Events()) != 0 || len(s.Messages()) != 0 {
		t.Fatal("空会话不应有事件或消息")
	}
}

func TestTotalUsageDerivedNotStored(t *testing.T) {
	dir := t.TempDir()
	jl, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	id := "usage"
	jl.SaveHeader(id, Header{ID: id})
	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventUser, Text: "q1"})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "a1", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}})
	mustAppend(t, s, Event{Kind: EventUser, Text: "q2"})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "a2", Usage: &llm.Usage{PromptTokens: 30, CompletionTokens: 6, TotalTokens: 36}})
	// 无用量的事件不参与折叠
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "a3"})

	got := s.TotalUsage()
	if got.PromptTokens != 40 || got.CompletionTokens != 10 || got.TotalTokens != 50 {
		t.Fatalf("TotalUsage = %+v, want 40/10/50", got)
	}

	// 派生而非存储：Restore 重放后自动重建，后端不感知聚合
	rs, err := Restore(id, jl)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rs.TotalUsage() != (llm.Usage{PromptTokens: 40, CompletionTokens: 10, TotalTokens: 50}) {
		t.Fatalf("恢复后 TotalUsage = %+v", rs.TotalUsage())
	}
}

// thinking 模式多轮对话必须原样回传思维链：投影不能丢 ReasoningContent。
func TestReasoningRoundTrip(t *testing.T) {
	dir := t.TempDir()
	jl, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	id := "think"
	jl.SaveHeader(id, Header{ID: id})
	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventUser, Text: "复杂问题"})
	mustAppend(t, s, Event{Kind: EventAssistant, Text: "答案", Reasoning: "先分解再求解"})

	msgs := s.Messages()
	if len(msgs) != 2 || msgs[1].ReasoningContent != "先分解再求解" {
		t.Fatalf("思维链应在投影中回传: %+v", msgs)
	}

	// 经持久化往返后仍在（thinking 服务端不接受丢字段的历史）
	rs, err := Restore(id, jl)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := rs.Messages(); len(got) != 2 || got[1].ReasoningContent != "先分解再求解" {
		t.Fatalf("恢复后思维链丢失: %+v", got)
	}
}

func TestNextTurn(t *testing.T) {
	s := New("turns", nil)
	if got := s.NextTurn(); got != 1 {
		t.Fatalf("空会话 NextTurn = %d, want 1", got)
	}
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Turn: 1, Text: "一"})
	mustAppend(t, s, Event{Kind: EventTurnEnd, Turn: 1, Reason: "completed"})
	if got := s.NextTurn(); got != 2 {
		t.Fatalf("NextTurn = %d, want 2", got)
	}
	// 序号由日志派生，乱序写入也取最大值
	mustAppend(t, s, Event{Kind: EventUser, Turn: 5, Text: "跳号"})
	if got := s.NextTurn(); got != 6 {
		t.Fatalf("NextTurn = %d, want 6（取日志最大值+1）", got)
	}
}

// turn/end 的错误详情：进日志、能落盘、且**不进模型投影**。
func TestTurnEndErrorDetail(t *testing.T) {
	dir := t.TempDir()
	jl, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	id := "err"
	if err := jl.SaveHeader(id, Header{ID: id}); err != nil {
		t.Fatalf("SaveHeader: %v", err)
	}

	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventTurnStart, Turn: 1})
	mustAppend(t, s, Event{Kind: EventUser, Text: "问"})
	mustAppend(t, s, Event{Kind: EventTurnEnd, Turn: 1, Reason: "error", Error: "模型不可用：证书过期"})

	// 落盘后详情仍在（审计凭据不能只剩"失败了"）
	events, err := jl.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	last := events[len(events)-1]
	if last.Reason != "error" || last.Error != "模型不可用：证书过期" {
		t.Fatalf("错误详情未落盘: %+v", last)
	}

	// 投影里没有它：模型历史不该被诊断信息污染
	for _, m := range s.Messages() {
		if m.Content == "模型不可用：证书过期" {
			t.Fatalf("错误详情不应进入模型投影: %+v", m)
		}
	}
}

// 错误详情超长要截断，且不能切出半个字符。
func TestErrorDetailTruncation(t *testing.T) {
	if got := ErrorDetail(nil); got != "" {
		t.Fatalf("nil 错误应返回空串, got %q", got)
	}
	if got := ErrorDetail(errors.New("短")); got != "短" {
		t.Fatalf("短错误应原样保留, got %q", got)
	}

	long := strings.Repeat("错", MaxErrorDetail) // 每个 3 字节
	got := ErrorDetail(errors.New(long))
	if !strings.HasSuffix(got, "…（已截断）") {
		t.Fatalf("超长应截断并标记: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断后必须是合法 UTF-8: %q", got)
	}
	// 截断后的内容应是原串的**前缀**（不多不少）
	body := strings.TrimSuffix(got, "…（已截断）")
	if !strings.HasPrefix(long, body) {
		t.Fatalf("截断内容应是原文前缀")
	}
	if len(body) > MaxErrorDetail {
		t.Fatalf("截断后长度 %d 超过上限 %d", len(body), MaxErrorDetail)
	}
}

// 日志是人读的：不能把 & < > 转义成 \u0026 之类（JSON 合法但全是噪声）。
func TestJSONLDoesNotEscapeHTML(t *testing.T) {
	dir := t.TempDir()
	jl, err := NewJSONL(dir)
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	id := "esc"
	if err := jl.SaveHeader(id, Header{ID: id}); err != nil {
		t.Fatalf("SaveHeader: %v", err)
	}
	s := New(id, jl)
	mustAppend(t, s, Event{Kind: EventToolCall, Turn: 1, ToolCall: &llm.ToolCall{
		ID: "c1", Name: "shell", Args: `{"command":"cd lab && go test ./... 2>&1"}`,
	}})

	raw, err := os.ReadFile(jl.path(id))
	if err != nil {
		t.Fatalf("读日志: %v", err)
	}
	if strings.Contains(string(raw), `\u0026`) || strings.Contains(string(raw), `\u003e`) {
		t.Fatalf("不该有 HTML 转义: %s", string(raw))
	}
	if !strings.Contains(string(raw), "&&") {
		t.Fatalf("&& 应原样出现: %s", string(raw))
	}
}

// 会话累计要**区分** 输入 / 缓存读 / 缓存写 / 输出——缓存命中单列才看得出
// "这次省了多少"（Anthropic 的输入本就不含缓存，OpenAI 的含）。
func TestTotalUsageDistinguishesCache(t *testing.T) {
	sess := New("t", nil)
	mustAppend(t, sess, Event{Kind: EventAssistant, Turn: 1, Usage: &llm.Usage{
		PromptTokens: 100, FreshInputTokens: 20, CompletionTokens: 5, TotalTokens: 105,
		Details: llm.UsageDetails{
			Input:  llm.InputTokenDetails{CacheRead: 80, CacheWrite: 3},
			Output: llm.OutputTokenDetails{Reasoning: 2},
		},
	}})
	mustAppend(t, sess, Event{Kind: EventAssistant, Turn: 1, Usage: &llm.Usage{
		PromptTokens: 50, FreshInputTokens: 50, CompletionTokens: 7, TotalTokens: 57,
		Details: llm.UsageDetails{Input: llm.InputTokenDetails{CacheRead: 0}},
	}})

	u := sess.TotalUsage()
	if u.FreshInputTokens != 70 {
		t.Fatalf("FreshInputTokens = %d, want 70", u.FreshInputTokens)
	}
	if u.Details.Input.CacheRead != 80 {
		t.Fatalf("CacheRead = %d, want 80", u.Details.Input.CacheRead)
	}
	if u.Details.Input.CacheWrite != 3 {
		t.Fatalf("CacheWrite = %d, want 3", u.Details.Input.CacheWrite)
	}
	if u.CompletionTokens != 12 {
		t.Fatalf("CompletionTokens = %d, want 12", u.CompletionTokens)
	}
	if u.TotalTokens != 162 {
		t.Fatalf("TotalTokens = %d, want 162", u.TotalTokens)
	}
	if u.Details.Output.Reasoning != 2 {
		t.Fatalf("Reasoning = %d, want 2", u.Details.Output.Reasoning)
	}
}

// 不变式：**分项之和 = 总计**，两个协议口径都要成立——
//   - OpenAI：prompt 含缓存 → total = prompt + completion；
//     拆开就是 新鲜输入 + 缓存读 + 输出。
//   - Anthropic：input 不含缓存 → total = input + 缓存写 + 缓存读 + 输出。
//
// 显示是"输入/缓存读/缓存写/输出/共"，若分项和 ≠ 共，用户看到的账就平不了。
func TestTotalUsagePartsSumToTotal(t *testing.T) {
	cases := []struct {
		name string
		ev   llm.Usage
	}{
		{"OpenAI 口径", llm.Usage{
			// prompt=1086（含 186 缓存命中）→ fresh=900
			PromptTokens: 1086, FreshInputTokens: 900, CompletionTokens: 84, TotalTokens: 1170,
			Details: llm.UsageDetails{Input: llm.InputTokenDetails{CacheRead: 186}},
		}},
		{"Anthropic 口径", llm.Usage{
			// input=900（不含缓存）→ 缓存写 3 + 缓存读 186 + 输出 84
			PromptTokens: 900, FreshInputTokens: 900, CompletionTokens: 84, TotalTokens: 1173,
			Details: llm.UsageDetails{Input: llm.InputTokenDetails{CacheRead: 186, CacheWrite: 3}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sess := New("t", nil)
			mustAppend(t, sess, Event{Kind: EventAssistant, Turn: 1, Usage: &c.ev})
			u := sess.TotalUsage()
			sum := u.FreshInput() + u.Details.Input.CacheRead +
				u.Details.Input.CacheWrite + u.CompletionTokens
			if sum != u.TotalTokens {
				t.Fatalf("分项和 %d ≠ 总计 %d（%+v）", sum, u.TotalTokens, u)
			}
		})
	}
}
