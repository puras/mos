package openai

import (
	"testing"

	pkgopenai "hegq.net/mogo/pkg/protocol/openai"
	"hegq.net/mos/llm"
)

func TestMediaURL(t *testing.T) {
	cases := []struct {
		part llm.ContentPart
		want string
	}{
		{llm.ContentPart{Type: "image", URL: "https://a.com/x.png", Data: "d", MIME: "image/png"}, "https://a.com/x.png"},
		{llm.ContentPart{Type: "image", Data: "aGk=", MIME: "image/png"}, "data:image/png;base64,aGk="},
		{llm.ContentPart{Type: "image", Data: "aGk="}, "data:image/png;base64,aGk="}, // 默认 MIME
	}
	for _, c := range cases {
		if got := mediaURL(c.part); got != c.want {
			t.Fatalf("mediaURL(%+v) = %q, want %q", c.part, got, c.want)
		}
	}
}

func TestAudioFormat(t *testing.T) {
	cases := map[string]string{
		"audio/mpeg": "mp3", "audio/mp3": "mp3",
		"audio/wav": "wav", "audio/x-wav": "wav",
		"audio/ogg": "ogg", "audio/aac": "aac",
		"application/octet-stream": "mp3", // 未知默认 mp3
	}
	for in, want := range cases {
		if got := audioFormat(in); got != want {
			t.Fatalf("audioFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToOpenAISystemContent(t *testing.T) {
	// 纯文本 → string 分支
	plain := toOpenAISystemContent(llm.Message{Role: llm.RoleSystem, Content: "约束"})
	flat, ok := plain.(*pkgopenai.OpenAIChatCompletionSystemMessageContentUnionFlat)
	if !ok || flat.Type != "string" || flat.String == nil || *flat.String != "约束" {
		t.Fatalf("plain = %+v", plain)
	}
	// 多模态 → parts 分支
	parts := toOpenAISystemContent(llm.Message{
		Role: llm.RoleSystem,
		ContentParts: []llm.ContentPart{
			{Type: "text", Text: "看"},
			{Type: "image", Data: "aGk=", MIME: "image/png"},
		},
	})
	flatParts, ok := parts.(*pkgopenai.OpenAIChatCompletionSystemMessageContentUnionFlat)
	if !ok || flatParts.Type != "parts" || flatParts.Parts == nil || len(*flatParts.Parts) != 2 {
		t.Fatalf("parts = %+v", parts)
	}
	// audio 块走 input_audio
	out := toOpenAIContent([]llm.ContentPart{{Type: "audio", Data: "aGk=", MIME: "audio/wav"}})
	af, ok := out[0].(*pkgopenai.OpenAIChatCompletionContentPartUnionFlat)
	if len(out) != 1 || !ok || af.Type != "input_audio" || af.InputAudio == nil {
		t.Fatalf("audio part = %+v", out)
	}
	if string(af.InputAudio.InputAudio.Format) != "wav" {
		t.Fatalf("audio format = %v", af.InputAudio.InputAudio.Format)
	}
}
