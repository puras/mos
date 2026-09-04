package gemini

import (
	"testing"

	"github.com/puras/mos/llm"
)

func TestToGeminiSchema(t *testing.T) {
	// nil → nil
	if s := toGeminiSchema(nil); s != nil {
		t.Fatalf("nil schema = %+v", s)
	}
	// map 转换
	s := toGeminiSchema(map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "integer"}},
	})
	if s == nil || s.Type != "object" || s.Properties == nil {
		t.Fatalf("schema = %+v", s)
	}
	// 坏 JSON（通道）→ nil，不 panic
	if s := toGeminiSchema(make(chan int)); s != nil {
		t.Fatalf("bad json schema = %+v", s)
	}
}

func TestGeminiParts(t *testing.T) {
	out := geminiParts([]llm.ContentPart{
		{Type: "text", Text: "hi"},
		{Type: "image", URL: "https://a.com/x.png", MIME: "image/png"}, // FileData
		{Type: "image", Data: "aGk=", MIME: "image/png"},               // InlineData
		{Type: "audio", Data: "aGk=", MIME: "audio/wav"},
		{Type: "file", URL: "https://a.com/f.pdf", MIME: "application/pdf"},
	})
	if len(out) != 5 {
		t.Fatalf("parts = %d, want 5", len(out))
	}
	if out[0].Text != "hi" {
		t.Fatalf("text part = %+v", out[0])
	}
	if out[1].FileData == nil || out[1].FileData.FileURI != "https://a.com/x.png" {
		t.Fatalf("file data part = %+v", out[1])
	}
	if out[2].InlineData == nil || string(out[2].InlineData.Data) != "hi" {
		t.Fatalf("inline part = %+v", out[2])
	}
	if out[4].FileData == nil {
		t.Fatalf("file part = %+v", out[4])
	}
}
