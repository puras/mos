package tool

import (
	"encoding/json"
	"testing"
)

type grepArgs struct {
	Root       string `json:"root,omitempty"`
	Pattern    string `json:"pattern"`
	Glob       string `json:"glob,omitempty"`
	MaxMatches int    `json:"max_matches,omitempty"`
}

func TestParseNormalizesConcatenatedJSON(t *testing.T) {
	// 模型偶发输出 "{...}{...}" 拼接，应取第一个完整 JSON 值
	raw := json.RawMessage(`{"root": "/x"}{"root": "/y", "pattern": "MOSS", "max_matches": 10}`)
	got, err := Parse[grepArgs](raw)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if got.Root != "/x" || got.Pattern != "" {
		t.Fatalf("unexpected parsed args: %+v", got)
	}
}

func TestParseNormalValid(t *testing.T) {
	raw := json.RawMessage(`{"root": "/x", "pattern": "MOSS", "max_matches": 10}`)
	got, err := Parse[grepArgs](raw)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if got.Root != "/x" || got.Pattern != "MOSS" || got.MaxMatches != 10 {
		t.Fatalf("unexpected parsed args: %+v", got)
	}
}

func TestParseEmptyArgs(t *testing.T) {
	got, err := Parse[grepArgs](nil)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if got != (grepArgs{}) {
		t.Fatalf("unexpected parsed args: %+v", got)
	}
}
