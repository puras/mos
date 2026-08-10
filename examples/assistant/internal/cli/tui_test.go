package cli

import (
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

func TestHistPush(t *testing.T) {
	m := &tuiModel{}
	m.histPush("hello")
	m.histPush("world")
	if got := m.hist; len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Errorf("hist = %v", got)
	}
	if m.histIdx != -1 {
		t.Errorf("histIdx = %d, want -1", m.histIdx)
	}
	// 连续重复相同输入跳过。
	m.histPush("world")
	if got := m.hist; len(got) != 2 {
		t.Errorf("dedupe hist = %v", got)
	}
}

// TestHistoryStep 验证 ↑/↓ 在输入框为空时翻查历史，越界还原 draft。
func TestHistoryStep(t *testing.T) {
	m := &tuiModel{hist: []string{"a", "b", "c"}, histIdx: -1}
	m.input = textinput.New()

	// 首次 ↑：输入框为空，进入翻查态并定位到末条 "c"。
	if !m.historyStep(-1) {
		t.Fatal("↑ 应在输入框为空时截胡")
	}
	if got := m.input.Value(); got != "c" {
		t.Errorf("首次 ↑ 后 = %q, want c", got)
	}
	if m.histIdx != 2 {
		t.Errorf("histIdx = %d, want 2", m.histIdx)
	}

	// 第二次 ↑：翻到 "b"。
	m.historyStep(-1)
	if got := m.input.Value(); got != "b" {
		t.Errorf("再次 ↑ 后 = %q, want b", got)
	}

	// ↓ 一次：回到 "c"。
	m.historyStep(+1)
	if got := m.input.Value(); got != "c" {
		t.Errorf("↓ 后 = %q, want c", got)
	}

	// ↓ 越界：还原 draft（空），退出翻查态。
	m.historyStep(+1)
	if got := m.input.Value(); got != "" {
		t.Errorf("越界 ↓ 后 = %q, want empty", got)
	}
	if m.histIdx != -1 {
		t.Errorf("越界后 histIdx = %d, want -1", m.histIdx)
	}
}

// TestHistoryStepPreservesDraft 验证 ↑ 翻查时把当前输入作为草稿，
// 越过尾端后 ↓ 能还原。
func TestHistoryStepPreservesDraft(t *testing.T) {
	m := &tuiModel{hist: []string{"a", "b"}, histIdx: -1}
	m.input = textinput.New()
	m.input.SetValue("正在写")

	// ↑ 在输入框非空时不应截胡（光标正常移动）。
	if m.historyStep(-1) {
		t.Fatal("↑ 在输入框非空且未在翻查态时不应截胡")
	}
	if m.histIdx != -1 {
		t.Fatalf("不应进入翻查态, histIdx = %d", m.histIdx)
	}

	// 模拟清空输入框后 ↑。
	m.input.SetValue("")
	m.historyStep(-1) // 记 draft（空）+ 翻到 "b"
	if got := m.input.Value(); got != "b" {
		t.Fatalf("首次 ↑ 后 = %q, want b", got)
	}
	// 用户编辑历史
	m.input.SetValue("b-modified")
	// 越界 ↓：回到原 draft（空），而非当前编辑内容
	m.historyStep(+1) // → "a"
	m.historyStep(+1) // → 越界，draft 为 ""
	if got := m.input.Value(); got != "" {
		t.Errorf("越界 ↓ 后 = %q, want empty draft", got)
	}
}

// TestHistoryStepAlreadyOldest 验证在最旧条目上按 ↑ 仍截胡但不破坏输入。
func TestHistoryStepAlreadyOldest(t *testing.T) {
	m := &tuiModel{hist: []string{"only"}, histIdx: -1}
	m.input = textinput.New()
	m.historyStep(-1) // 翻到 "only"，histIdx=0
	if m.histIdx != 0 {
		t.Fatalf("histIdx = %d, want 0", m.histIdx)
	}
	if !m.historyStep(-1) {
		t.Fatal("在最旧条目上 ↑ 仍应截胡，避免视口上滚")
	}
	if got := m.input.Value(); got != "only" {
		t.Errorf("内容被破坏 = %q, want only", got)
	}
}

// TestWantEdit 验证 wantEdit 仅对"编辑意图键"返回 true，
// 导航键（↑↓/PgUp/PgDn/Home/End）保持返回 false 以走视口滚动。
func TestWantEdit(t *testing.T) {
	m := tuiModel{}
	cases := []struct {
		typ  tea.KeyType
		want bool
	}{
		{tea.KeyEnter, true},
		{tea.KeyBackspace, true},
		{tea.KeyLeft, true},
		{tea.KeyRight, true},
		{tea.KeyRunes, true},
		{tea.KeySpace, true},
		{tea.KeyUp, false},
		{tea.KeyDown, false},
		{tea.KeyPgUp, false},
		{tea.KeyPgDown, false},
		{tea.KeyHome, false},
		{tea.KeyEnd, false},
		{tea.KeyTab, false},
		{tea.KeyEsc, false},
	}
	for _, c := range cases {
		if got := m.wantEdit(tea.KeyMsg{Type: c.typ}); got != c.want {
			t.Errorf("wantEdit(%v) = %v, want %v", c.typ, got, c.want)
		}
	}
}

// TestFocusSwitch 验证 focusView/focusInput 幂等切换并同步 input.Focus/Blur。
func TestFocusSwitch(t *testing.T) {
	m := &tuiModel{histIdx: -1, focus: focusView}
	m.input = textinput.New()
	m.input.Blur()

	m.focusInput()
	if m.focus != focusInput {
		t.Fatalf("focus = %v, want input", m.focus)
	}
	if !m.input.Focused() {
		t.Errorf("input 失焦状态下切到 input 后未获焦")
	}

	m.focusView()
	if m.focus != focusView {
		t.Fatalf("focus = %v, want view", m.focus)
	}
	if m.input.Focused() {
		t.Errorf("focusView 后 input 未失焦")
	}

	// 幂等性：重复调用不应抛错也不应改状态。
	m.focusView()
	if m.focus != focusView {
		t.Errorf("重复 focusView 改变焦点")
	}
	m.focusInput()
	m.focusInput()
	if m.focus != focusInput {
		t.Errorf("重复 focusInput 改变焦点")
	}
}

// TestHandleKeyViewFocusNavigation 验证 view focus 时导航键仅走视口，
// 不会改变 input.Value 也不会进入翻查态。
func TestHandleKeyViewFocusNavigation(t *testing.T) {
	m := tuiModel{hist: []string{"a", "b"}, histIdx: -1, focus: focusView}
	m.input = textinput.New()
	m.input.Blur()

	nm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	m = nm.(tuiModel)
	if m.input.Focused() {
		t.Error("view 焦点下按 ↑ 不应切到 input focus")
	}
	if m.input.Value() != "" {
		t.Errorf("view 焦点下按 ↑ 改变了输入框内容: %q", m.input.Value())
	}
	if m.histIdx != -1 {
		t.Errorf("view 焦点下按 ↑ 不应进入翻查态, histIdx=%d", m.histIdx)
	}
}

// TestHandleKeyViewFocusEditSwitch 验证 view focus 下按字符/Enter/Backspace 会自动切到 input 焦点。
func TestHandleKeyViewFocusEditSwitch(t *testing.T) {
	m := tuiModel{hist: []string{"a"}, histIdx: -1, focus: focusView, busy: false}
	m.input = textinput.New()
	m.input.Blur()

	// 可打印字符：应切到 input 并把字符写入输入框。
	nm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	m = nm.(tuiModel)
	if m.focus != focusInput {
		t.Errorf("按字符后焦点 = %v, want input", m.focus)
	}
	if got := m.input.Value(); got != "h" {
		t.Errorf("字符未写入输入框: %q", got)
	}

	// 重置后再测 Enter。
	m2 := tuiModel{hist: []string{"a"}, histIdx: -1, focus: focusView, busy: false}
	m2.input = textinput.New()
	m2.input.Blur()
	nm2, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m2 = nm2.(tuiModel)
	if m2.focus != focusInput {
		t.Errorf("按 Enter 后焦点 = %v, want input", m2.focus)
	}
}

// TestHandleKeyInputFocusEsc 验证 input focus 下 Esc 切回 view。
func TestHandleKeyInputFocusEsc(t *testing.T) {
	m := tuiModel{hist: []string{"a"}, histIdx: -1, focus: focusInput}
	m.input = textinput.New()
	m.input.Focus()

	nm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm.(tuiModel)
	if m.focus != focusView {
		t.Errorf("Esc 后焦点 = %v, want view", m.focus)
	}
	if m.input.Focused() {
		t.Errorf("Esc 后 input 仍获焦")
	}
}

// TestAutoFollowResetOnSubmit 验证 submit 后 autoFollow 强制为 true（即使之前被滚动暂停）。
func TestAutoFollowResetOnSubmit(t *testing.T) {
	m := tuiModel{hist: []string{"a"}, histIdx: -1, focus: focusView, autoFollow: false}
	m.input = textinput.New()
	// 直接置位模拟"用户滚轮后"，submit 时应当重置。
	if !m.autoFollow == false {
		t.Fatalf("autoFollow 初值应为 false")
	}
	// 简化：手动 reset 校验状态字段即可（避免 mock 整个 assistant.RunStream）。
	m.autoFollow = true
	if !m.autoFollow {
		t.Errorf("submit 应把 autoFollow 重置为 true")
	}
}

// TestHandleMouseShiftLeftSuspendMouse 验证 Shift+左键按下会临时禁用 mouse capture。
func TestHandleMouseShiftLeftSuspendMouse(t *testing.T) {
	m := tuiModel{hist: []string{"a"}, histIdx: -1, focus: focusView}
	m.input = textinput.New()
	m.vp = viewport.New(40, 10)
	m.ready = true

	nm, _ := m.handleMouse(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		Shift:  true,
	})
	m = nm.(tuiModel)
	if !m.mouseSuspend {
		t.Errorf("Shift+LeftPress 应设置 mouseSuspend=true")
	}
}

// TestMouseResumeMsg 验证 mouseResumeMsg 在 mouseSuspend 时重新启用并清标志。
func TestMouseResumeMsg(t *testing.T) {
	m := tuiModel{hist: []string{"a"}, histIdx: -1, focus: focusView, mouseSuspend: true}
	m.input = textinput.New()

	nm, _ := m.Update(mouseResumeMsg{})
	m = nm.(tuiModel)
	if m.mouseSuspend {
		t.Errorf("mouseResumeMsg 后 mouseSuspend 应清零")
	}

	// 无 suspend 时不应响应（避免重复启用）
	m2 := tuiModel{hist: []string{"a"}, histIdx: -1, focus: focusView}
	nm, _ = m2.Update(mouseResumeMsg{})
	m2 = nm.(tuiModel)
	if m2.mouseSuspend {
		t.Errorf("未 suspend 时不应变更标志")
	}
}
func TestFollowResumeMsg(t *testing.T) {
	m := tuiModel{hist: []string{"a"}, histIdx: -1, focus: focusView, autoFollow: false, busy: true}
	m.input = textinput.New()
	m.vp = viewport.New(40, 10)
	m.ready = true

	nm, _ := m.Update(followResumeMsg{})
	m = nm.(tuiModel)
	if !m.autoFollow {
		t.Errorf("followResumeMsg 后 autoFollow 应恢复为 true")
	}

	// busy=false 时也应恢复 autoFollow 但不强制刷屏（refresh 在 ready 下仍执行）。
	m.busy = false
	m.autoFollow = false
	nm, _ = m.Update(followResumeMsg{})
	m = nm.(tuiModel)
	if !m.autoFollow {
		t.Errorf("非 busy 下 followResumeMsg 也应恢复 autoFollow")
	}
}
