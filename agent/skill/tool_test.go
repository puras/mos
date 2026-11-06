package skill

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRecorder 是假的技能事件记录器。
type fakeRecorder struct {
	mu    sync.Mutex
	calls int
	name  string
	dir   string
	err   error
	// mismatchName / missing 是 allowed-tools 求交诊断（S8.4-D）。
	mismatchName string
	missing      []string
}

func (r *fakeRecorder) RecordActivate(name, description, dir string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.name = name
	r.dir = dir
	return r.err
}

// RecordAllowedToolsMismatch 实现 skill.Recorder（记 allowed-tools 求交诊断）。
func (r *fakeRecorder) RecordAllowedToolsMismatch(name string, missing []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mismatchName = name
	r.missing = append([]string(nil), missing...)
	return nil
}

// twoSkills 装两个技能，返回清单。
func twoSkills(t *testing.T) *Catalog {
	t.Helper()
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "deploy"), "name: deploy\ndescription: 部署流程", "步骤 1")
	writeSkill(t, filepath.Join(root, "review"), "name: review\ndescription: 代码评审", "步骤 2")
	return Load(Options{Dirs: []string{root}})
}

// 工具按 name 返回正文。
func TestToolReturnsSkillBody(t *testing.T) {
	cat := twoSkills(t)
	tool := NewTool(cat)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"deploy"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("返回类型 = %T", out)
	}
	content, _ := m["content"].(string)
	if !strings.Contains(content, "步骤 1") {
		t.Fatalf("正文 = %q", content)
	}
	if m["name"] != "deploy" {
		t.Fatalf("name = %v", m["name"])
	}
}

// 名字不存在：报错（fail-closed，不返回空正文）。
func TestToolUnknownSkillFails(t *testing.T) {
	tool := NewTool(twoSkills(t))
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"nope"}`)); err == nil {
		t.Fatal("不存在的技能应报错")
	} else if !strings.Contains(err.Error(), "不在当前技能目录里") {
		t.Fatalf("err = %v", err)
	}
}

// 参数缺失：报错。
func TestToolRequiresName(t *testing.T) {
	tool := NewTool(twoSkills(t))
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("缺 name 应报错")
	}
}

// 仅用户可调用的技能：模型调用一律拒绝（fail-closed）。
func TestToolRejectsModelInvocationOfUserOnlySkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "user"), "name: user-only\ndescription: 仅用户\ndisable-model-invocation: true", "正文")
	cat := Load(Options{Dirs: []string{root}})
	tool := NewTool(cat)

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"user-only"}`))
	if err == nil {
		t.Fatal("仅用户可调用的技能不该让模型加载")
	}
	if !strings.Contains(err.Error(), "仅限用户显式调用") {
		t.Fatalf("err = %v", err)
	}
}

// 加载成功 → 状态记激活 + 事件留痕。
func TestToolRecordsActivation(t *testing.T) {
	cat := twoSkills(t)
	st := NewState()
	rec := &fakeRecorder{}
	ctx := WithRecorder(WithState(context.Background(), st), rec)

	if _, err := NewTool(cat).Execute(ctx, json.RawMessage(`{"name":"deploy"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := st.Activated(); len(got) != 1 || got[0] != "deploy" {
		t.Fatalf("激活状态 = %v", got)
	}
	if rec.calls != 1 || rec.name != "deploy" {
		t.Fatalf("事件 = %d 次 %q", rec.calls, rec.name)
	}
	if rec.dir == "" {
		t.Fatal("事件应带技能目录")
	}
}

// 事件写失败 → 不返回正文，且状态不变（审计链不能断在这里）。
func TestToolFailsWhenRecorderFails(t *testing.T) {
	cat := twoSkills(t)
	st := NewState()
	rec := &fakeRecorder{err: errors.New("会话写失败")}
	ctx := WithRecorder(WithState(context.Background(), st), rec)

	out, err := NewTool(cat).Execute(ctx, json.RawMessage(`{"name":"deploy"}`))
	if err == nil {
		t.Fatalf("审计写失败应报错（out=%v）", out)
	}
	if out != nil {
		t.Fatalf("审计写失败不该返回正文：%v", out)
	}
	if got := st.Activated(); len(got) != 0 {
		t.Fatalf("审计写失败不该记激活：%v", got)
	}
}

// 用户显式手势：/name 展开成正文，参数追加在正文之后。
func TestExpandInvocation(t *testing.T) {
	cat := twoSkills(t)

	out, ok, err := cat.ExpandInvocation("/deploy 生产环境")
	if err != nil {
		t.Fatalf("ExpandInvocation: %v", err)
	}
	if !ok {
		t.Fatal("应命中手势")
	}
	if !strings.Contains(out, "步骤 1") {
		t.Fatalf("应展开正文：\n%s", out)
	}
	if !strings.Contains(out, "生产环境") {
		t.Fatalf("参数应追加在正文之后：\n%s", out)
	}
}

// 未命中：原样返回，不是错误。
func TestExpandInvocationUnknown(t *testing.T) {
	cat := twoSkills(t)
	out, ok, err := cat.ExpandInvocation("/nope 参数")
	if err != nil {
		t.Fatalf("ExpandInvocation: %v", err)
	}
	if ok {
		t.Fatal("不存在的技能不该展开")
	}
	if out != "/nope 参数" {
		t.Fatalf("原文应保留：%q", out)
	}
}

// 仅用户可调用的技能：用户手势可以展开（这正是该标记的用途）。
func TestExpandInvocationWorksForUserOnlySkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "user"), "name: user-only\ndescription: 仅用户\ndisable-model-invocation: true", "正文")
	cat := Load(Options{Dirs: []string{root}})

	out, ok, err := cat.ExpandInvocation("/user-only")
	if err != nil {
		t.Fatalf("ExpandInvocation: %v", err)
	}
	if !ok {
		t.Fatal("用户手势应能展开仅用户可调用的技能")
	}
	if !strings.Contains(out, "正文") {
		t.Fatalf("应展开正文：\n%s", out)
	}
}

// 非手势文本不动（框架不扫全文）。
func TestExpandInvocationIgnoresPlainText(t *testing.T) {
	cat := twoSkills(t)
	in := "帮我看看 /deploy 这个技能"
	if _, ok, _ := cat.ExpandInvocation(in); ok {
		t.Fatal("句中的 /deploy 不该被展开")
	}
}

// 状态：重复激活只记一次；并发安全。
func TestStateActivateOnce(t *testing.T) {
	st := NewState()
	if !st.Activate("deploy") {
		t.Fatal("首次激活应返回 true")
	}
	if st.Activate("deploy") {
		t.Fatal("重复激活应返回 false")
	}
	if got := st.Activated(); len(got) != 1 {
		t.Fatalf("激活 = %v", got)
	}
}

func TestStateConcurrentActivate(t *testing.T) {
	st := NewState()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st.Activate("deploy")
		}()
	}
	wg.Wait()
	if got := st.Activated(); len(got) != 1 {
		t.Fatalf("并发激活应只留一条：%v", got)
	}
}

// ctx 里的状态与记录器：能取回，取不到就是 nil（不 panic）。
func TestStateContextRoundTrip(t *testing.T) {
	st := NewState()
	rec := &fakeRecorder{}
	ctx := WithRecorder(WithState(context.Background(), st), rec)
	if StateFrom(ctx) != st {
		t.Fatal("状态应能取回")
	}
	if RecorderFrom(ctx) != rec {
		t.Fatal("记录器应能取回")
	}
	if StateFrom(context.Background()) != nil || RecorderFrom(context.Background()) != nil {
		t.Fatal("没挂就是 nil")
	}
}
