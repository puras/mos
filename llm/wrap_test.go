package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeModel 是测试用的可编程模型。
type fakeModel struct {
	calls  int
	err    error
	resp   *Response
	cap    Capability
	slow   time.Duration // >0 时 Chat 阻塞该时长（可取消）
	stream func(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error)
}

func (m *fakeModel) Capabilities() Capability {
	if m.cap != 0 {
		return m.cap
	}
	return CapFunctionCalling
}

func (m *fakeModel) Chat(ctx context.Context, _ *Request) (*Response, error) {
	m.calls++
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.slow):
	}
	if m.err != nil {
		return nil, m.err
	}
	return m.resp, nil
}

func (m *fakeModel) ChatStream(ctx context.Context, req *Request, emit func(StreamChunk) error) (*Response, error) {
	m.calls++
	if m.stream != nil {
		return m.stream(ctx, req, emit)
	}
	if m.err != nil {
		return nil, m.err
	}
	return m.resp, nil
}

func TestWithLogging(t *testing.T) {
	base := &fakeModel{resp: &Response{Message: Message{Role: RoleAssistant, Content: "ok"}}}
	var got []CallResult
	m := WithLogging(base, func(r CallResult) { got = append(got, r) })

	req := &Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	if _, err := m.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, err := m.ChatStream(context.Background(), req, nil); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("callbacks = %d, want 2", len(got))
	}
	if got[0].Req != req || got[0].Resp.Message.Content != "ok" || got[0].Err != nil || got[0].Stream {
		t.Fatalf("callback[0] = %+v", got[0])
	}
	if !got[1].Stream {
		t.Fatalf("callback[1] stream = false, want true")
	}
}

func TestWithRetry(t *testing.T) {
	base := &fakeModel{resp: &Response{}}
	m := WithRetry(base, 3, time.Millisecond)
	base.err = errors.New("boom")
	if _, err := m.Chat(context.Background(), &Request{}); err == nil {
		t.Fatal("Chat should fail after exhausted retries")
	}
	if base.calls != 3 {
		t.Fatalf("calls = %d, want 3", base.calls)
	}
	base.err = nil
	if _, err := m.Chat(context.Background(), &Request{}); err != nil {
		t.Fatalf("Chat after recovery: %v", err)
	}
}

func TestWithRetryStreamPassthrough(t *testing.T) {
	base := &fakeModel{resp: &Response{}}
	m := WithRetry(base, 3, time.Millisecond)
	base.err = errors.New("boom")
	if _, err := m.ChatStream(context.Background(), &Request{}, nil); err == nil {
		t.Fatal("ChatStream should not retry and return error")
	}
	if base.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on stream)", base.calls)
	}
}

func TestWithTimeout(t *testing.T) {
	base := &fakeModel{slow: 50 * time.Millisecond}
	m := WithTimeout(base, time.Millisecond)
	if _, err := m.Chat(context.Background(), &Request{}); err == nil {
		t.Fatal("Chat should exceed timeout")
	}
}

func TestWithFallback(t *testing.T) {
	primary := &fakeModel{err: errors.New("down")}
	backup := &fakeModel{resp: &Response{Message: Message{Content: "backup"}}}
	m := WithFallback(primary, backup)
	resp, err := m.Chat(context.Background(), &Request{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "backup" {
		t.Fatalf("content = %q, want backup", resp.Message.Content)
	}
	// 主模型正常时不用备用
	primary.err = nil
	primary.resp = &Response{Message: Message{Content: "primary"}}
	if resp, _ := m.Chat(context.Background(), &Request{}); resp.Message.Content != "primary" {
		t.Fatalf("content = %q, want primary", resp.Message.Content)
	}
}

func TestWithCache(t *testing.T) {
	base := &fakeModel{resp: &Response{Message: Message{Content: "cached"}}}
	m := WithCache(base, time.Minute)
	req := &Request{Messages: []Message{{Role: RoleUser, Content: "q"}}}
	if _, err := m.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, err := m.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat (cached): %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("calls = %d, want 1 (cache hit)", base.calls)
	}
	// 不同请求不命中缓存
	if _, err := m.Chat(context.Background(), &Request{Messages: []Message{{Role: RoleUser, Content: "other"}}}); err != nil {
		t.Fatalf("Chat (different req): %v", err)
	}
	if base.calls != 2 {
		t.Fatalf("calls = %d, want 2", base.calls)
	}
}

// TestWrappersCapabilitiesPassthrough：装饰器能力位透传/合并正确（组合链可被路由层识别）。
func TestWrappersCapabilitiesPassthrough(t *testing.T) {
	base := &fakeModel{cap: CapStreaming | CapFunctionCalling}

	if got := WithLogging(base, nil).Capabilities(); got != CapStreaming|CapFunctionCalling {
		t.Fatalf("logging caps = %v", got)
	}
	if got := WithRetry(base, 1, 0).Capabilities(); got != CapStreaming|CapFunctionCalling {
		t.Fatalf("retry caps = %v", got)
	}
	if got := WithTimeout(base, time.Second).Capabilities(); got != CapStreaming|CapFunctionCalling {
		t.Fatalf("timeout caps = %v", got)
	}
	if got := WithCache(base, time.Minute).Capabilities(); got != CapStreaming|CapFunctionCalling {
		t.Fatalf("cache caps = %v", got)
	}
	// fallback 合并主备能力
	backup := &fakeModel{cap: CapStructuredOutput}
	if got := WithFallback(base, backup).Capabilities(); got != CapStreaming|CapFunctionCalling|CapStructuredOutput {
		t.Fatalf("fallback caps = %v", got)
	}
}

func TestWithTimeoutStream(t *testing.T) {
	base := &fakeModel{
		slow: 50 * time.Millisecond,
		stream: func(ctx context.Context, _ *Request, _ func(StreamChunk) error) (*Response, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
				return &Response{}, nil
			}
		},
	}
	m := WithTimeout(base, time.Millisecond)
	if _, err := m.ChatStream(context.Background(), &Request{}, nil); err == nil {
		t.Fatal("ChatStream should exceed timeout")
	}
}

func TestWithFallbackStream(t *testing.T) {
	primary := &fakeModel{
		err: errors.New("down"),
		stream: func(context.Context, *Request, func(StreamChunk) error) (*Response, error) {
			return nil, errors.New("stream down")
		},
	}
	backup := &fakeModel{resp: &Response{Message: Message{Role: RoleAssistant, Content: "backup"}}}
	m := WithFallback(primary, backup)
	resp, err := m.ChatStream(context.Background(), &Request{}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Message.Content != "backup" {
		t.Fatalf("content = %q, want backup", resp.Message.Content)
	}
}

func TestWithCacheStreamPassthrough(t *testing.T) {
	base := &fakeModel{resp: &Response{Message: Message{Content: "ok"}}}
	m := WithCache(base, time.Minute)
	req := &Request{Messages: []Message{{Role: RoleUser, Content: "q"}}}
	if _, err := m.ChatStream(context.Background(), req, nil); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if _, err := m.ChatStream(context.Background(), req, nil); err != nil {
		t.Fatalf("ChatStream (2nd): %v", err)
	}
	// ChatStream 不缓存：两次都落到底层
	if base.calls != 2 {
		t.Fatalf("calls = %d, want 2 (stream not cached)", base.calls)
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	base := &fakeModel{resp: &Response{Message: Message{Content: "v1"}}}
	m := WithCache(base, 5*time.Millisecond)
	req := &Request{Messages: []Message{{Role: RoleUser, Content: "q"}}}
	if _, err := m.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := m.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat (after expiry): %v", err)
	}
	if base.calls != 2 {
		t.Fatalf("calls = %d, want 2 (cache expired)", base.calls)
	}
}
