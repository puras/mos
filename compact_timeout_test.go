package mos_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/puras/mos/agent"
	"github.com/puras/mos/llm"
	"github.com/puras/mos/llm/anthropic"
)

// 端到端：超时要能在**真实传输链**上被认出来。
//
// 复现的正是线上那个场景——宿主的轮次 ctx 只有 cancel 没有 deadline，网关
// 半开（上游不回应）时摘要请求会永远挂着。这里用"永不回应的服务器"当网关：
// 摘要必须在 SummaryTimeout 内以明确的"超时"收尾，而不是把整轮挂住，
// 也不是甩一个裸的 context canceled 让宿主去反推是谁掐的。
func TestCompactorSummaryTimeoutThroughRealTransport(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // 上游挂起：不回响应头
	}))
	defer func() {
		close(block)
		srv.Close()
	}()

	m, err := anthropic.New(llm.Config{
		"api_key":  "test-key",
		"base_url": srv.URL,
		"model":    "claude-3-5-sonnet",
	})
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}
	c := agent.NewLLMCompactor(m, agent.CompactorConfig{
		ContextWindow:  1000,
		SummaryTimeout: 100 * time.Millisecond,
	})

	start := time.Now()
	_, _, err = c.Summarize(context.Background(),
		[]llm.Message{{Role: llm.RoleUser, Content: "很长的历史"}}, "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("挂起的网关应让摘要超时")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("超时没生效，白等了 %s", elapsed)
	}
	msg := err.Error()
	t.Logf("超时错误文案: %v", err)
	if !strings.Contains(msg, "超时") {
		t.Fatalf("错误应说明是超时: %v", err)
	}
	if !strings.Contains(msg, "context deadline exceeded") {
		t.Fatalf("错误应保留超时原因: %v", err)
	}
	if strings.Contains(msg, "context canceled") {
		t.Fatalf("超时不该报成 context canceled: %v", err)
	}
}
