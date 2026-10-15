package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/puras/mos/tool"
)

func TestDefaultRegistersCleanly(t *testing.T) {
	r := tool.NewRegistry()
	for _, tl := range Default() {
		if err := r.Register(tl); err != nil {
			t.Fatalf("内置工具 %q 注册失败: %v", tl.Name(), err)
		}
	}
	if names := r.Names(); len(names) == 0 || names[0] != "current_time" {
		t.Fatalf("Names = %v", names)
	}
	// 每个工具的 schema 都要能生成（反射不炸）
	for _, def := range r.List() {
		if def.Parameters == nil {
			t.Fatalf("%s 未生成 schema", def.Name)
		}
	}
}

func TestCurrentTime(t *testing.T) {
	out, err := CurrentTime{}.Execute(context.Background(), json.RawMessage(`{"timezone":"UTC"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	m := out.(map[string]any)
	if m["timezone"] != "UTC" {
		t.Fatalf("timezone = %v, want UTC", m["timezone"])
	}
	for _, k := range []string{"time", "weekday", "unix", "iso8601"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("缺字段 %q: %+v", k, m)
		}
	}
}

// 无效时区必须报错（fail-closed），不静默回落本地时区。
func TestCurrentTimeInvalidTimezone(t *testing.T) {
	_, err := CurrentTime{}.Execute(context.Background(), json.RawMessage(`{"timezone":"Not/AZone"}`))
	if err == nil {
		t.Fatal("无效时区应报错")
	}
}

// 留空时区用本地时区。
func TestCurrentTimeLocalFallback(t *testing.T) {
	out, err := CurrentTime{}.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.(map[string]any)["timezone"] == "" {
		t.Fatal("留空时区应回落到本地时区")
	}
}
