package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

// FR-C2 守护测试：schema 输出不得包含 title / $defs（现状天然满足——
// generateSchema 递归内联展开；此测试锁住，防止未来有人引入）。
func TestSchemaHasNoTitleOrDefs(t *testing.T) {
	type Inner struct {
		Name string `json:"name" mos:"desc=名称"`
		N    int    `json:"n"`
	}
	type Args struct {
		Query  string   `json:"query" mos:"desc=查询,required=true"`
		Tags   []string `json:"tags,omitempty"`
		Inner  Inner    `json:"inner"`
		Pointers []*Inner `json:"pointers,omitempty"`
	}
	s := generateSchema(Args{})
	b, _ := json.Marshal(s)
	out := string(b)
	if strings.Contains(out, "title") {
		t.Fatalf("schema 不该有 title: %s", out)
	}
	if strings.Contains(out, "$defs") || strings.Contains(out, "definitions") {
		t.Fatalf("schema 不该有 $defs/definitions: %s", out)
	}
	if !strings.Contains(out, "required") {
		t.Fatalf("required 丢了: %s", out)
	}
}
