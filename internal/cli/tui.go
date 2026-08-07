package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"hegq.net/mos/pkg/agent"
	"hegq.net/mos/pkg/assistant"
)

// tuiEvent 是智能体运行协程流向 TUI 界面的消息（应用层展示数据源）。
type tuiEvent struct {
	ev    agent.Event // 过程事件（思考/工具等）
	log   string      // 模型调用日志行
	done  bool        // 任务结束标记
	reply string      // 最终回复
	err   error       // 任务错误
}

// tuiMsg 将 tuiEvent 包装为 tea.Msg。
type tuiMsg struct{ e tuiEvent }

// tuiEntry 是对话/过程区的一条展示条目。
type tuiEntry struct {
	kind     string // user | think | stream | tool | result | done | log | info | err
	title    string // 收起态摘要（think）或单行内容（其余）
	body     string // think/stream/done 的 markdown 正文
	open     bool   // think 是否展开
	streamed bool   // 由流式增量生成（最终回复转正时用于去重判断）
	// ansi/ansiW 是 body 的 markdown 渲染缓存；dirty 标记流式追加后需重渲染。
	ansi  string
	ansiW int
	dirty bool
}

// streamState 记录进行中的流式条目（模型正在产出文本）。
type streamState struct {
	idx int // 进行中条目在 entries 中的下标
}

// ---- 展示样式（应用层展示策略，仅界面内使用） ----

// TUI 界面样式由 applyTheme(themeKind) 按主题初始化（见 theme.go）。
var (
	userStyle  lipgloss.Style
	agentStyle lipgloss.Style
	toolStyle  lipgloss.Style
	logStyle   lipgloss.Style
	infoStyle  lipgloss.Style
	errStyle   lipgloss.Style
	// thinkBox 思考折叠卡片：小标题条（Trae/ChatGPT 风格），
	// 点击展开后正文缩进显示在其下方。
	thinkBox lipgloss.Style
)

// tuiModel 是 bubbletea 全屏界面模型。
type tuiModel struct {
	a       *assistant.Assistant
	sess    *agent.Session
	ctx     context.Context
	events  chan tuiEvent
	entries []tuiEntry
	entryY  []int // 各展示条目的起始行号（视口内容坐标），用于鼠标点击定位
	input   textinput.Model
	vp      viewport.Model
	busy    bool // 智能体正在运行
	ready   bool // 视口是否初始化
	width   int
	height  int
	pending *agent.Event // 缓存的思考事件（最终轮与回复去重）
	stream  *streamState // 进行中的流式条目（打字机效果）
	// streamedIdx 最近一次由流式增量定稿为"思考"的条目下标（-1 表示无）。
	// 最终回复到达时据此转正，避免依赖"最后一条"（中间可能插入日志行等）。
	streamedIdx int
	// mdRenderer/mdW 是 markdown 渲染器（按宽度懒创建）。
	mdRenderer *glamour.TermRenderer
	mdW        int
	mdTheme    themeKind // 当前渲染器对应的主题，主题切换后重建
	// hist 已提交过的非空非命令输入，按提交顺序追加。
	// histIdx 是当前翻查位置（-1 表示不在翻查态）；
	// histDraft 是进入翻查前的当前草稿，用于 ↓ 越过尾端后还原。
	hist      []string
	histIdx   int
	histDraft string
	// focus 当前键盘焦点：view 走视口滚动；input 走文本输入与历史翻查。
	// 默认 view，键入/Enter 自动切到 input，Esc 切回。
	focus focusKind
	// autoFollow 视口是否自动跟随最新内容（默认 true）。
	// 用户在 busy 期间主动滚轮时临时置 false，followResumeMsg 时恢复。
	// 这样用户在生成中向上查看历史不会被下次刷新强制拉回底部。
	autoFollow bool
	// theme 当前配色主题（dark/light），启动时按终端背景自动检测，/theme 可切换。
	theme themeKind
	// mouseSuspend Shift+拖动期间临时禁用 mouse capture，3 秒后定时器自动恢复。
	mouseSuspend bool
}

// focusKind 区分两区焦点，避免输入区按键与视口滚动互相干扰。
type focusKind int

const (
	focusView  focusKind = iota // 展示区焦点：↑↓/PgUp/PgDn 等滚视口
	focusInput                  // 输入区焦点：字符编辑、↑↓翻历史、Enter 提交
)

// helpText 底部帮助行文案。
// 鼠标策略：智能体运行时捕获鼠标（滚轮滚动/点击思考展开），
// 任务完成数秒后自动释放，交还终端以支持拖动选中复制文本。
const helpText = "展示区: ↑↓/PgUp/PgDn/滚轮 滚动 · 点击思考展开/收起 · Shift+拖动 = 选中复制 · 输入区: 键入自动进入，↑↓ 翻历史，Enter 提交，Esc 返回"

// mouseResumeMsg Shift+拖动 临时禁用 mouse 后，定时器到期时恢复启用，
// 以保证后续滚轮/点击重新归 bubbletea 处理。
type mouseResumeMsg struct{}

// followResumeMsg 触发恢复视口自动跟随（用户在生成中滚动后暂停一段时间再恢复）。
type followResumeMsg struct{}

// runTUI 启动全屏交互界面（应用层展示能力）。
// 事件通道在此创建，logSink/eventSink 被接管为通道转发，退出后恢复默认输出。
// 展示策略（思考去重/收起展开/样式）全部在界面内实现，框架层只发原始事件。
func runTUI(a *assistant.Assistant, modelName string) error {
	events := make(chan tuiEvent, 64)
	prevLog, prevEvent := logSink, eventSink
	logSink = func(line string) { events <- tuiEvent{log: line} }
	eventSink = func(ev agent.Event) { events <- tuiEvent{ev: ev} }
	defer func() { logSink, eventSink = prevLog, prevEvent }()

	m := tuiModel{
		a:          a,
		sess:       a.NewSession(),
		ctx:        context.Background(),
		events:     events,
		histIdx:    -1,
		focus:      focusView,
		autoFollow: true,
		theme:      detectTheme(),
	}
	applyTheme(m.theme)
	m.input = textinput.New()
	m.input.Prompt = "你 > "
	m.input.PromptStyle = userStyle
	m.input.Placeholder = "键入内容开始对话 · Esc 回展示区"
	m.input.PlaceholderStyle = lipgloss.NewStyle().Faint(true)
	m.input.CharLimit = 4096
	// 默认视口焦点（输入框失焦）；键入/Enter 自动切到输入焦点。
	m.input.Blur()
	m.refresh()
	m.info("模型: " + modelName + " · 工具: " + strings.Join(a.ToolNames(), ", "))

	// mouse 全程启用，保证滚轮在 idle/busy 期间都能滚动视口、点击思考可展开。
	// 牺牲了默认的拖动选中复制：需要选中时按 Shift+拖动 临时交给终端接管。
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (m tuiModel) Init() tea.Cmd {
	// 启动即启用 mouse capture，让滚轮在 idle 也能用。
	return tea.Batch(textinput.Blink, func() tea.Msg { return tea.EnableMouseCellMotion() })
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		w, h := msg.Width-4, msg.Height-8
		if w < 10 {
			w = 10
		}
		if h < 3 {
			h = 3
		}
		if !m.ready {
			m.vp = viewport.New(w, h)
			m.ready = true
		} else {
			m.vp.Width, m.vp.Height = w, h
		}
		m.input.Width = w
		m.refresh()
		return m, nil
	case tuiMsg:
		return m.handleEvent(msg.e)
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case mouseResumeMsg:
		// Shift+拖动 临时禁用 mouse 的定时器到期，重新启用让滚轮/点击恢复。
		if m.mouseSuspend {
			m.mouseSuspend = false
			return m, func() tea.Msg { return tea.EnableMouseCellMotion() }
		}
		return m, nil
	case followResumeMsg:
		// 恢复视口自动跟随：仅在仍处于运行中且用户未再次滚动时生效。
		// 恢复后立即 GotoBottom 并刷新，让后续增量继续从底部展示。
		m.autoFollow = true
		if m.busy {
			m.refresh()
		}
		return m, nil
	}
	return m, nil
}

func (m tuiModel) handleEvent(e tuiEvent) (tea.Model, tea.Cmd) {
	switch {
	case e.done:
		m.busy = false
		m.pending = nil
		m.autoFollow = true
		m.input.Placeholder = "键入内容继续 · Esc 回展示区"
		if e.err != nil {
			// 运行出错：进行中的流式条目转为错误行
			if m.stream != nil {
				m.entries[m.stream.idx].kind = "err"
				m.entries[m.stream.idx].title = "执行出错: " + e.err.Error()
				m.stream = nil
			}
			m.streamedIdx = -1
			m.add("err", "执行出错: "+e.err.Error())
		} else {
			m.promoteStream(e.reply)
		}
		m.refresh()
		return m, m.poll()
	case e.log != "":
		m.add("log", e.log)
		m.refresh()
		return m, m.poll()
	default:
		m.appendEvent(e.ev)
		m.refresh()
		return m, m.poll()
	}
}

// promoteStream 收尾流式回复：
// 最近一轮流式思考若内容即最终回复，直接转正为回复条目，
// 避免"思考 + 回复"重复展示；否则按非流式路径追加回复条目。
func (m *tuiModel) promoteStream(reply string) {
	if m.streamedIdx >= 0 && m.streamedIdx < len(m.entries) {
		e := &m.entries[m.streamedIdx]
		if e.kind == "think" && (reply == "" || strings.TrimSpace(e.body) == strings.TrimSpace(reply)) {
			e.kind = "done"
			e.title = "智能体 >" // 正文（markdown）保留在 body
			e.open = false
			m.streamedIdx = -1
			m.stream = nil
			return
		}
	}
	m.streamedIdx = -1
	m.stream = nil
	if reply != "" {
		m.add("done", "智能体 >", reply)
	}
}

func (m tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Ctrl+C 全局退出；Esc 始终回展示区。
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	if msg.Type == tea.KeyEsc && m.focus == focusInput {
		m.focusView()
		return m, nil
	}

	if m.busy {
		// 任务运行中：所有键盘输入仅用于滚动展示区。
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}

	// 输入区焦点：完整由 textinput 处理（含 historyStep）。
	if m.focus == focusInput {
		switch msg.Type {
		case tea.KeyEnter:
			line := strings.TrimSpace(m.input.Value())
			if line == "" {
				return m, nil
			}
			m.input.SetValue("")
			m.histPush(line)
			if strings.HasPrefix(line, "/") {
				return m.command(line)
			}
			return m.submit(line)
		case tea.KeyUp:
			if m.historyStep(-1) {
				return m, nil
			}
		case tea.KeyDown:
			if m.historyStep(+1) {
				return m, nil
			}
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// 展示区焦点：区分"导航键（滚视口）"与"编辑键（切到输入区）"。
	if m.wantEdit(msg) {
		// 切到输入区并把当前按键转发，让用户立即看到字符/编辑生效。
		m.focusInput()
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// wantEdit 判定在展示区焦点下该键是否应触发输入区。
// 规则：可打印字符、Enter、Backspace、左右方向键触发；其余（↑↓/PgUp/PgDn/Tab/Home/End 等）保持视口滚动。
func (m tuiModel) wantEdit(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEnter, tea.KeyBackspace, tea.KeyLeft, tea.KeyRight:
		return true
	case tea.KeyRunes, tea.KeySpace:
		return true
	}
	return false
}

// focusView 切回展示区焦点：input 失焦，状态栏文字提示。
func (m *tuiModel) focusView() {
	if m.focus == focusView {
		return
	}
	m.focus = focusView
	m.input.Blur()
	m.refresh()
}

// focusInput 切到输入区焦点：input 获焦。
func (m *tuiModel) focusInput() {
	if m.focus == focusInput {
		return
	}
	m.focus = focusInput
	m.input.Focus()
	m.refresh()
}

// historyStep 处理 ↑/↓ 翻查历史。返回 true 表示已截胡该键（不应再交给 input/vp）。
// dir 为 -1 表示上一条（↑），+1 表示下一条（↓）。
func (m *tuiModel) historyStep(dir int) bool {
	switch {
	case dir < 0:
		// ↑：输入框为空时进入翻查态并定位到末尾；翻查态下继续翻上一条。
		if m.histIdx == -1 && m.input.Value() != "" {
			return false
		}
		if m.histIdx == -1 {
			m.histDraft = m.input.Value()
			m.histIdx = len(m.hist)
		}
		if m.histIdx > 0 {
			m.histIdx--
			m.input.SetValue(m.hist[m.histIdx])
			m.input.CursorEnd()
			m.refresh()
			return true
		}
		// 已是最旧一条，截胡避免视口继续上滚；输入框保持当前值。
		return true
	case dir > 0:
		// ↓：仅在翻查态下生效；越界时回到 draft 并退出翻查。
		if m.histIdx < 0 {
			return false
		}
		m.histIdx++
		if m.histIdx >= len(m.hist) {
			m.histIdx = -1
			m.input.SetValue(m.histDraft)
			m.input.CursorEnd()
		} else {
			m.input.SetValue(m.hist[m.histIdx])
			m.input.CursorEnd()
		}
		m.refresh()
		return true
	}
	return false
}

// histPush 提交后写入历史，连续重复相同输入会跳过；翻查态复位。
func (m *tuiModel) histPush(line string) {
	if n := len(m.hist); n > 0 && m.hist[n-1] == line {
		m.histIdx = -1
		m.histDraft = ""
		return
	}
	m.hist = append(m.hist, line)
	m.histIdx = -1
	m.histDraft = ""
}

// handleMouse 处理鼠标交互：
//  - 滚轮：交给 viewport，并暂停 auto-follow
//  - Shift + 左键按下：临时禁用 mouse capture，让终端接管拖动选中复制，
//    3 秒后定时器自动恢复 mouse（用户拖选完成后能继续滚轮/点击）
//  - 左键点击输入区切焦点到 input；点击思考行（▸/▾）展开/收起
func (m tuiModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action == tea.MouseActionPress {
		switch msg.Button {
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			// 用户主动滚动：暂停自动跟随一段窗口，避免后续流式增量
			// 强制 GotoBottom 把视口拉回底部导致 iTerm2 上"滚出 TUI"的感觉。
			// 期间后续事件仍能刷新，但保持用户滚到的位置。
			m.autoFollow = false
			return m, tea.Batch(cmd, m.followResumeLater())
		}
		// Shift + 左键按下 → 临时禁用 mouse，让终端接管本次拖选（复制文本）。
		if msg.Shift && msg.Button == tea.MouseButtonLeft && !m.mouseSuspend {
			m.mouseSuspend = true
			return m, tea.Batch(
				func() tea.Msg { return tea.DisableMouse() },
				tea.Tick(3*time.Second, func(time.Time) tea.Msg { return mouseResumeMsg{} }),
			)
		}
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if !m.ready {
		return m, nil
	}
	// 点击底部输入框区域：切到输入区焦点。
	if msg.Y >= m.height-1 {
		m.focusInput()
		return m, nil
	}
	top := m.topLines()
	if msg.Y < top || msg.Y >= top+m.vp.Height {
		return m, nil
	}
	if len(m.entryY) == 0 {
		return m, nil
	}
	// 屏幕行 → 视口内容行：内容行 = 滚动偏移 + 视口内相对行
	contentY := m.vp.YOffset + (msg.Y - top)
	for i := range m.entryY {
		if contentY < m.entryY[i] {
			break
		}
		if i+1 == len(m.entryY) || contentY < m.entryY[i+1] {
			if m.entries[i].kind == "think" {
				m.entries[i].open = !m.entries[i].open
				m.refresh()
			}
			break
		}
	}
	return m, nil
}

// followResumeLater 用户滚动后，2 秒内未再次滚动则恢复自动跟随。
func (m *tuiModel) followResumeLater() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return followResumeMsg{} })
}

// topLines 视口上方占用的行数（横幅 + 帮助行），用于鼠标点击的屏幕坐标换算。
func (m tuiModel) topLines() int {
	header := tuiHeader.Render("MoS Assistant — 智能体 CLI")
	help := infoStyle.Render(helpText)
	return strings.Count(header, "\n") + strings.Count(help, "\n") + 2
}

// submit 发起一次任务：展示用户消息，后台协程运行智能体，事件经通道流入界面。
func (m tuiModel) submit(line string) (tea.Model, tea.Cmd) {
	m.add("user", line)
	m.busy = true
	m.autoFollow = true
	m.focusView()
	m.input.Placeholder = "思考中…"
	m.streamedIdx = -1
	m.refresh()
	go func() {
		reply, err := m.a.RunStream(m.ctx, m.sess, line)
		m.events <- tuiEvent{done: true, reply: reply, err: err}
	}()
	// 启用鼠标捕获（滚轮滚动/点击思考展开）；任务完成数秒后自动释放
	// mouse 全程启用：不再在 submit/done 时切换 capture。
	return m, m.poll()
}

func (m tuiModel) command(line string) (tea.Model, tea.Cmd) {
	switch {
	case line == "/exit" || line == "/quit" || line == "/q":
		return m, tea.Quit
	case line == "/reset" || line == "/new":
		m.sess = m.a.NewSession()
		m.info("（已开启新会话）")
	case line == "/tools":
		m.info("可用工具: " + strings.Join(m.a.ToolNames(), ", "))
	case line == "/theme":
		if m.theme == themeDark {
			m.theme = themeLight
		} else {
			m.theme = themeDark
		}
		applyTheme(m.theme)
		// 主题切换：重建 markdown 渲染器并刷新界面（输入框提示色同步）。
		m.mdRenderer = nil
		m.mdTheme = m.theme
		m.input.PromptStyle = userStyle
		m.refresh()
		m.info("已切换为 " + themeName(m.theme) + " 主题")
	case line == "/help":
		m.info("命令: /exit /reset /tools /theme · ↑↓ 翻历史 · Esc 回展示区 · 点击思考展开/收起")
	default:
		m.add("err", "未知命令: "+line)
	}
	m.refresh()
	return m, nil
}

// poll 阻塞读取智能体事件通道并包装为 tea.Msg。
func (m tuiModel) poll() tea.Cmd {
	return func() tea.Msg { return tuiMsg{e: <-m.events} }
}

// appendEvent 将框架层过程事件转换为展示条目。
// 最终作答轮的思考与最终回复内容相同，去重展示（应用层展示策略）。
// 流式路径：模型增量（EventStream）先进入"生成中"条目，思考定稿时转正，
// 最终回复到达后由 promoteStream 去重收尾。
func (m *tuiModel) appendEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventStream:
		if ev.Delta == "" {
			return
		}
		if m.stream == nil {
			m.add("stream", "")
			m.stream = &streamState{idx: len(m.entries) - 1}
		}
		e := &m.entries[m.stream.idx]
		e.body += ev.Delta
		e.dirty = true // 内容变化，markdown 需重渲染
	case agent.EventStep:
		if m.stream != nil {
			// 思考已流式展示过：把生成中条目定稿为思考（收起），不重复追加
			m.finalizeStream("think", fmt.Sprintf("第%d轮 思考", ev.Turn))
			return
		}
		m.pending = &ev
	case agent.EventToolCall:
		if m.stream != nil {
			m.finalizeStream("think", fmt.Sprintf("第%d轮 思考", ev.Turn))
		}
		m.flushThink("")
		m.add("tool", fmt.Sprintf("第%d轮 调用工具 %s(%s)", ev.Turn, ev.ToolCall.Name, summarize(ev.ToolCall.Args, 60)))
	case agent.EventToolResult:
		m.flushThink("")
		m.add("result", fmt.Sprintf("第%d轮 工具结果: %s", ev.Turn, summarize(fmt.Sprint(ev.ToolResult), 80)))
	case agent.EventDone:
		m.flushThink(ev.Content)
	case agent.EventFact:
		// 已验证事实已写入任务状态，界面不重复展示
	}
}

// finalizeStream 将进行中的流式条目定稿为指定类型（思考等），结束"生成中"状态。
// 定稿为思考时默认展开：流式内容已展示过，突然收起为纯标题会显得突兀。
func (m *tuiModel) finalizeStream(kind, title string) {
	if m.stream == nil {
		return
	}
	e := &m.entries[m.stream.idx]
	e.kind = kind
	e.title = title
	e.open = kind == "think" // 思考保留内容可见；其余（如转错误行）不展开
	e.streamed = true
	if kind == "think" {
		m.streamedIdx = m.stream.idx // 记录候选，最终回复到达时转正
	}
	m.stream = nil
}

// flushThink 将缓存的思考写入展示条目。
// 最终作答轮的思考与回复内容相同：不展示内容摘要（避免与回复重复），
// 只收起为提示行，点击箭头展开后可见完整内容。
func (m *tuiModel) flushThink(skip string) {
	if m.pending == nil {
		return
	}
	ev := *m.pending
	m.pending = nil
	if ev.Content == skip {
		m.add("think", fmt.Sprintf("第%d轮 思考", ev.Turn), ev.Content)
		return
	}
	m.add("think", fmt.Sprintf("第%d轮 %s", ev.Turn, summarize(ev.Content, 80)), ev.Content)
}

func (m *tuiModel) add(kind, title string, body ...string) {
	e := tuiEntry{kind: kind, title: title}
	if len(body) > 0 {
		e.body = body[0]
	}
	m.entries = append(m.entries, e)
}

func (m *tuiModel) info(s string) { m.add("info", s) }

// refresh 重算视口内容与条目行号映射；autoFollow 或原本在底部时保持自动跟随。
// 用户在 busy 期间主动滚轮会临时关闭 autoFollow，期间不强制 GotoBottom，
// 避免每次刷新把视口拉回底部（iTerm2 等终端上会让用户感觉"滚出去了"）。
func (m *tuiModel) refresh() {
	if !m.ready {
		return
	}
	var b strings.Builder
	entryY := make([]int, 0, len(m.entries))
	y := 0
	for i := range m.entries {
		entryY = append(entryY, y)
		s := m.renderEntry(&m.entries[i], m.vp.Width)
		b.WriteString(s)
		b.WriteString("\n\n")
		y += strings.Count(s, "\n") + 3
	}
	m.entryY = entryY
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
	if atBottom || m.autoFollow {
		m.vp.GotoBottom()
	}
}

func (m *tuiModel) renderEntry(e *tuiEntry, width int) string {
	w := width - 4
	if w < 10 {
		w = 40
	}
	switch e.kind {
	case "user":
		return userStyle.Width(w).Render("你 > " + e.title)
	case "think":
		// Trae/ChatGPT 折叠卡片：深灰标题条 + 可点击展开/收起，正文缩进显示。
		mark := "▸"
		if e.open {
			mark = "▾"
		}
		head := thinkBox.Render(mark + " 💭 " + e.title)
		if !e.open || e.body == "" {
			return head
		}
		return head + "\n" + indentBody(m.renderMD(e, w))
	case "tool":
		return toolStyle.Width(w).Render("  ▶ " + e.title)
	case "result":
		return toolStyle.Width(w).Render("  ◀ " + e.title)
	case "done":
		head := agentStyle.Render(e.title)
		if e.body == "" {
			return head
		}
		return head + "\n" + m.renderMD(e, w)
	case "stream":
		// 生成中的流式思考：卡片标题"思考中…" + 打字机内容，与定稿后的
		// think 卡片视觉一致（定稿后由 finalizeStream 转成 think 卡片）。
		head := thinkBox.Render("⟳ 思考中…")
		if e.body == "" {
			return head + "…▋"
		}
		return head + "\n" + indentBody(m.renderMD(e, w)) + "▋"
	case "log":
		return logStyle.Width(w).Render("  · " + e.title)
	case "info":
		return infoStyle.Width(w).Render(e.title)
	case "err":
		return errStyle.Width(w).Render(e.title)
	}
	return e.title
}

// renderMD 渲染条目正文 markdown（带缓存，避免每帧全量重渲染）。
func (m *tuiModel) renderMD(e *tuiEntry, w int) string {
	if e.body == "" {
		return ""
	}
	if !e.dirty && e.ansi != "" && e.ansiW == w {
		return e.ansi
	}
	if m.mdRenderer == nil || m.mdW != w || m.mdTheme != m.theme {
		r, err := newMDRenderer(m.theme, w)
		if err != nil {
			m.mdRenderer = nil
			m.mdW = 0
			e.ansi = e.body
			e.ansiW = w
			e.dirty = false
			return e.body
		}
		m.mdRenderer = r
		m.mdW = w
		m.mdTheme = m.theme
	}
	out, err := m.mdRenderer.Render(e.body)
	if err != nil {
		e.ansi = e.body
	} else {
		e.ansi = strings.TrimRight(out, "\n")
	}
	e.ansiW = w
	e.dirty = false
	return e.ansi
}

func (m tuiModel) View() string {
	if !m.ready {
		return ""
	}
	header := tuiHeader.Render("MoS Assistant — 智能体 CLI")
	help := infoStyle.Render(helpText)
	footer := m.input.View()
	if m.busy {
		if m.stream != nil {
			footer = infoStyle.Render("⟳ 生成中…")
		} else {
			footer = infoStyle.Render("⟳ 思考中…")
		}
	} else {
		footer = renderInputBox(m.input.View(), m.focus, m.vp.Width)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, help, m.vp.View(), footer)
}

// inputBoxActive / inputBoxInactive 区分输入区焦点状态的边框样式（由 applyTheme 初始化）。
var (
	inputBoxActive   lipgloss.Style
	inputBoxInactive lipgloss.Style
	// tuiHeader 界面顶部横幅样式（由 applyTheme 初始化）。
	tuiHeader lipgloss.Style
)

// renderInputBox 给输入区加顶部分隔线，当前焦点时高亮。
func renderInputBox(input string, f focusKind, w int) string {
	style := inputBoxInactive
	if f == focusInput {
		style = inputBoxActive
	}
	if w < 2 {
		w = 2
	}
	return style.Width(w - 2).Render(input)
}

// summarize 取文本首行并截断到 n 个字符（按 rune），超长以 … 结尾。
func summarize(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// indentBody 给多行正文每行加统一缩进（2 空格），与折叠卡片标题条左对齐。
func indentBody(s string) string {
	if s == "" {
		return s
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
