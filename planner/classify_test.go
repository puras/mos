package planner

import (
	"context"
	"errors"
	"testing"

	"hegq.net/mos/llm"
)

type classifyModel struct {
	content string
	err     error
}

func (m *classifyModel) Capabilities() llm.Capability { return llm.CapFunctionCalling }
func (m *classifyModel) Chat(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: m.content}}, nil
}
func (m *classifyModel) ChatStream(context.Context, *llm.Request, func(llm.StreamChunk) error) (*llm.Response, error) {
	return nil, llm.ErrUnsupported
}

func TestParseClassification(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"driver":"react"}`, DriverReact},
		{`{"driver":"plan-execute"}`, DriverPlanExec},
		{`{"driver":"reflection"}`, DriverReflection},
		{"```json\n{\"driver\":\"plan-execute\"}\n```", DriverPlanExec},
		{"不是 JSON", ""},
		{`{"driver":"unknown"}`, ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseClassification(c.in); got != c.want {
			t.Fatalf("parseClassification(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClassifierModelError(t *testing.T) {
	c := NewClassifier(&classifyModel{err: errors.New("boom")})
	if _, err := c.Classify(context.Background(), "任务", nil); err == nil {
		t.Fatal("Classify should fail on model error")
	}
}
