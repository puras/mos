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

func TestFromContextStreamDefault(t *testing.T) {
	got := FromContextStream(context.Background())
	if got != DefaultStream {
		t.Fatalf("未注入时应返回 DefaultStream，got %p", got)
	}
	// 钉住流式兜底与 Default 的本质区别：不得带硬超时（会截断长流式输出）。
	if DefaultStream.Timeout != 0 {
		t.Fatalf("DefaultStream.Timeout = %v，流式兜底不得带硬超时", DefaultStream.Timeout)
	}
}

func TestFromContextStreamWithInjected(t *testing.T) {
	custom := &http.Client{}
	ctx := WithClient(context.Background(), custom)
	if got := FromContextStream(ctx); got != custom {
		t.Fatalf("注入后应返回 custom，got %p", got)
	}
	// 同一注入 key：工具层解析与流式解析取到同一实例。
	if got := FromContext(ctx); got != custom {
		t.Fatalf("FromContext 注入后应返回同一 custom，got %p", got)
	}
}
