package httpx

import (
	"context"
	"net/http"
	"testing"
)

func TestFromContextDefault(t *testing.T) {
	if got := FromContext(context.Background()); got != Default {
		t.Fatalf("未注入时应返回 Default，got %p", got)
	}
}

func TestFromContextWithInjected(t *testing.T) {
	custom := &http.Client{}
	ctx := WithClient(context.Background(), custom)
	if got := FromContext(ctx); got != custom {
		t.Fatalf("注入后应返回 custom，got %p", got)
	}
}

func TestDefaultHasTimeout(t *testing.T) {
	// 钉住兜底客户端与 http.DefaultClient 的本质区别：必须带超时。
	if Default.Timeout <= 0 {
		t.Fatalf("Default.Timeout = %v，兜底客户端必须带超时", Default.Timeout)
	}
}
