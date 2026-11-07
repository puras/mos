package subagent

import (
	"context"
	"testing"
)

// 回执校验：状态必须在三档里。
func TestResultValidateStatus(t *testing.T) {
	r := Result{Status: "done", Summary: "好了"}
	if err := r.Validate(); err == nil {
		t.Fatal("非法状态应报错")
	}
	for _, s := range []Status{StatusCompleted, StatusError, StatusTimeout} {
		res := Result{Status: s, Summary: "好了"}
		if s != StatusCompleted {
			res.Error = "为什么失败"
		}
		if err := res.Validate(); err != nil {
			t.Fatalf("状态 %q 应合法：%v", s, err)
		}
	}
}

// 失败状态必须带原因（父模型得知道为什么）。
func TestResultValidateRequiresError(t *testing.T) {
	r := Result{Status: StatusError, Summary: "没干成"}
	if err := r.Validate(); err == nil {
		t.Fatal("失败回执缺 error 说明应报错")
	}
}

// 线索必须带 summary。
func TestResultValidateFindings(t *testing.T) {
	r := Result{Status: StatusCompleted, Summary: "好了", Findings: []Finding{{Ref: "a.go:12"}}}
	if err := r.Validate(); err == nil {
		t.Fatal("缺 summary 的线索应报错")
	}
}

// RunnerFunc：宿主不想写结构体时的接缝。
func TestRunnerFunc(t *testing.T) {
	var got Spec
	r := RunnerFunc(func(_ context.Context, spec Spec) (Result, error) {
		got = spec
		return Result{Status: StatusCompleted, Summary: "干完了"}, nil
	})
	res, err := r.Run(context.Background(), Spec{Name: "reviewer", Task: "干活"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Status != StatusCompleted || res.Summary != "干完了" {
		t.Fatalf("回执不对：%+v", res)
	}
	if got.Task != "干活" {
		t.Fatalf("spec 没传到位：%+v", got)
	}
}
