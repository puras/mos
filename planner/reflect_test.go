package planner

import "testing"

func TestParseReviewJSON(t *testing.T) {
	r := parseReview(`{"issues":["缺细节","有错别字"],"score":70}`)
	if len(r.Issues) != 2 || r.Issues[0] != "缺细节" || r.Score != 70 {
		t.Fatalf("review = %+v", r)
	}
}

func TestParseReviewPass(t *testing.T) {
	r := parseReview(`{"issues":[],"score":90}`)
	if len(r.Issues) != 0 || r.Score != 90 {
		t.Fatalf("review = %+v, want passed", r)
	}
}

func TestParseReviewNoScore(t *testing.T) {
	r := parseReview(`{"issues":["x"]}`)
	if len(r.Issues) != 1 || r.Score != -1 {
		t.Fatalf("review = %+v, want score -1 (未提供)", r)
	}
}

func TestParseReviewFencedJSON(t *testing.T) {
	r := parseReview("```json\n{\"issues\":[\"一步\"],\"score\":80}\n```")
	if len(r.Issues) != 1 || r.Issues[0] != "一步" || r.Score != 80 {
		t.Fatalf("review = %+v", r)
	}
}

func TestParseReviewFreeText(t *testing.T) {
	// 非 JSON 回退：整段文本作为一条 issue（有批评即修订）
	r := parseReview("这段产出缺少具体数据支撑")
	if len(r.Issues) != 1 || r.Issues[0] != "这段产出缺少具体数据支撑" {
		t.Fatalf("review = %+v", r)
	}
}

func TestParseReviewEmpty(t *testing.T) {
	r := parseReview("   \n")
	if len(r.Issues) != 0 {
		t.Fatalf("review = %+v, want passed on empty", r)
	}
}
