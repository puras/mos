package anthropic

import (
	"strings"
	"testing"

	pkganthropic "hegq.net/mogo/pkg/protocol/anthropic"
	"hegq.net/mos/llm"
)

func TestAnthropicContent(t *testing.T) {
	out := anthropicContent([]llm.ContentPart{
		{Type: "text", Text: "看图"},
		{Type: "image", URL: "https://a.com/x.png", MIME: "image/png"},
		{Type: "image", Data: "aGk=", MIME: "image/jpeg"},
		{Type: "file", Data: "aGk=", MIME: "application/pdf"},
	})
	if len(out) != 4 {
		t.Fatalf("blocks = %d, want 4", len(out))
	}
	text, ok := out[0].(pkganthropic.AnthropicTextBlockParam)
	if !ok || text.Text != "看图" {
		t.Fatalf("text block = %+v", out[0])
	}
	imgURL, ok := out[1].(pkganthropic.AnthropicImageBlockParam)
	if !ok || imgURL.Source == nil {
		t.Fatalf("image url block = %+v", out[1])
	}
	imgB64, ok := out[2].(pkganthropic.AnthropicImageBlockParam)
	if !ok || imgB64.Source == nil {
		t.Fatalf("image base64 block = %+v", out[2])
	}
	doc, ok := out[3].(pkganthropic.AnthropicDocumentBlockParam)
	if !ok || doc.Source == nil {
		t.Fatalf("file block = %+v", out[3])
	}
}

func TestAnthropicInputSchema(t *testing.T) {
	schema := toAnthropicInputSchema(map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "integer"}},
		"required":   []any{"a"},
		"title":      "Args",
	})
	if schema.Type != "object" {
		t.Fatalf("type = %q", schema.Type)
	}
	if schema.Properties == nil {
		t.Fatal("properties should be copied")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "a" {
		t.Fatalf("required = %v", schema.Required)
	}
	if schema.ExtraFields == nil || schema.ExtraFields["title"] != "Args" {
		t.Fatalf("extra fields = %v", schema.ExtraFields)
	}
	// nil / 坏输入退化
	if s := toAnthropicInputSchema(nil); s.Type != "object" || s.Properties != nil {
		t.Fatalf("nil schema = %+v", s)
	}
}

func TestAnthropicInputSchemaBadJSON(t *testing.T) {
	// 不可序列化输入应退化为基础 schema 而不 panic
	s := toAnthropicInputSchema(make(chan int))
	if s.Type != "object" {
		t.Fatalf("type = %q", s.Type)
	}
}

func TestAnthropicContentBlockText(t *testing.T) {
	// 文本块内容透传
	out := anthropicContent([]llm.ContentPart{{Type: "text", Text: strings.Repeat("a", 5)}})
	text, ok := out[0].(pkganthropic.AnthropicTextBlockParam)
	if len(out) != 1 || !ok || text.Text != strings.Repeat("a", 5) {
		t.Fatalf("block = %+v", out)
	}
}
