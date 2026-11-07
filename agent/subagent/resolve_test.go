package subagent

import (
	"strings"
	"testing"
)

// 实际工具面 = 声明白名单 ∩ 父可见工具集（deny 优先：父看不见的进不来）。
func TestResolveToolsIntersectsWithParent(t *testing.T) {
	got := ResolveTools([]string{"read", "grep", "write"}, []string{"read", "grep", "ls"})
	if len(got) != 2 || got[0] != "grep" || got[1] != "read" {
		t.Fatalf("交集不对：%v", got)
	}
}

// 声明里父看不见的工具要被剔掉（声明不能放大宿主策略）。
func TestResolveToolsDropsToolsParentCannotSee(t *testing.T) {
	got := ResolveTools([]string{"read", "shell"}, []string{"read"})
	if len(got) != 1 || got[0] != "read" {
		t.Fatalf("父看不见的工具应被剔掉：%v", got)
	}
}

// 白名单为空 → 不收窄（继承父的可见集）。
func TestResolveToolsEmptyDeclaredInheritsParent(t *testing.T) {
	got := ResolveTools(nil, []string{"ls", "read"})
	if len(got) != 2 || got[0] != "ls" || got[1] != "read" {
		t.Fatalf("空白名单应继承父可见集：%v", got)
	}
}

// 父可见集为空（宿主没给）→ 退化为只按声明白名单。
func TestResolveToolsWithoutParentVisible(t *testing.T) {
	got := ResolveTools([]string{"read"}, nil)
	if len(got) != 1 || got[0] != "read" {
		t.Fatalf("应退化为只按声明：%v", got)
	}
}

// 声明了却没装的模型 → 报错（假能力，不静默降级）。
func TestCheckModelRejectsUnknownModel(t *testing.T) {
	def := Definition{Name: "reviewer", Model: "fast"}
	probe := func(model string) bool { return model == "slow" }
	err := CheckModel(def, probe)
	if err == nil {
		t.Fatal("宿主目录里没有该模型时应报错")
	}
	if !strings.Contains(err.Error(), "fast") {
		t.Fatalf("报错应点名模型：%v", err)
	}
}

// 模型在宿主目录里 → 通过。
func TestCheckModelAcceptsKnownModel(t *testing.T) {
	def := Definition{Name: "reviewer", Model: "fast"}
	probe := func(model string) bool { return model == "fast" }
	if err := CheckModel(def, probe); err != nil {
		t.Fatalf("模型存在时不该报错：%v", err)
	}
}

// 没声明模型 / 没接探针 → 不校验（宿主自己管）。
func TestCheckModelSkipsWhenUnset(t *testing.T) {
	if err := CheckModel(Definition{Name: "x"}, func(string) bool { return false }); err != nil {
		t.Fatalf("没声明模型不该报错：%v", err)
	}
	if err := CheckModel(Definition{Name: "x", Model: "fast"}, nil); err != nil {
		t.Fatalf("没接探针不该报错：%v", err)
	}
}

// 拆分判据：三者都没变 → 给诊断（只提示，不拦）。
func TestSplitDiagnosticFlagsNoDifference(t *testing.T) {
	def := Definition{Name: "clone"}
	msg := SplitDiagnostic(def, ParentProfile{Tools: []string{"read", "grep"}})
	if msg == "" {
		t.Fatal("与父无差别时应有诊断")
	}
	if !strings.Contains(msg, "clone") {
		t.Fatalf("诊断应点名子代理：%q", msg)
	}
}

// 拆分判据：instructions / tools / model 任一不同即可。
func TestSplitDiagnosticAcceptsAnyDifference(t *testing.T) {
	parent := ParentProfile{Tools: []string{"read"}, Model: "base"}

	withInstructions := Definition{Name: "a", HasInstructions: true}
	if msg := SplitDiagnostic(withInstructions, parent); msg != "" {
		t.Fatalf("instructions 非空应算有差别：%q", msg)
	}

	withTools := Definition{Name: "b", Tools: []string{"read", "grep"}}
	if msg := SplitDiagnostic(withTools, parent); msg != "" {
		t.Fatalf("白名单不同应算有差别：%q", msg)
	}

	withModel := Definition{Name: "c", Model: "fast"}
	if msg := SplitDiagnostic(withModel, parent); msg != "" {
		t.Fatalf("模型覆盖应算有差别：%q", msg)
	}
}

// 白名单与父可见集完全相同 → 不算差别（没收窄）。
func TestSplitDiagnosticSameToolsIsNoDifference(t *testing.T) {
	parent := ParentProfile{Tools: []string{"read", "grep"}}
	def := Definition{Name: "d", Tools: []string{"grep", "read"}}
	if msg := SplitDiagnostic(def, parent); msg == "" {
		t.Fatal("白名单与父相同应算无差别")
	}
}

// 模型覆盖与父相同 → 不算差别。
func TestSplitDiagnosticSameModelIsNoDifference(t *testing.T) {
	parent := ParentProfile{Model: "base"}
	def := Definition{Name: "e", Model: "base"}
	if msg := SplitDiagnostic(def, parent); msg == "" {
		t.Fatal("模型与父相同应算无差别")
	}
}
