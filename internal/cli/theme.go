package cli

import (
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// themeKind 界面配色主题：dark 适配深色终端背景，light 适配浅色背景。
type themeKind int

const (
	themeDark themeKind = iota
	themeLight
)

// 高对比 24-bit 配色（深色/浅色主题共用同一套色板，仅背景/前景互换）。
const (
	cInk      = "#e8e3d9" // 主前景：亮米色
	cUser     = "#87d7ff" // 用户：亮青
	cAgent    = "#c792ea" // 智能体：亮紫
	cTool     = "#ff9e64" // 工具：亮橙
	cThink    = "#ffe082" // 思考/标题：亮黄
	cSuccess  = "#b9f6ca" // 成功/H2：亮绿
	cErr      = "#ff7b8a" // 错误：亮红
	cMuted    = "#9aa0a6" // 静默辅助文字：灰
	cLine     = "#5d5d5d" // 分割线/边框
	cCodeBG   = "#0f1219" // 代码块背景（深色主题）
	cCodeFG   = "#d2a8ff" // 代码前景：亮紫
	cChipFG   = "#1a1b26" // 亮黄底上的前景：深蓝近黑
	cPanelBG  = "#1a1b26" // 面板/输入框背景
)

func detectTheme() themeKind {
	if termenv.HasDarkBackground() {
		return themeDark
	}
	return themeLight
}

func themeName(t themeKind) string {
	if t == themeLight {
		return "Light"
	}
	return "Dark"
}

// hexStr 把 24-bit 十六进制颜色字符串包装成 *string 给 glamour。
func hexStr(s string) *string { return &s }

// uBool 包装 bool 指针给 glamour。
func uBool(b bool) *bool { return &b }

// uUint 包装 uint 指针给 glamour。
func uUint(v uint) *uint { return &v }

// applyTheme 按主题重建 TUI 界面样式（用户/智能体/工具/错误/思考卡片/输入框/横幅）。
func applyTheme(t themeKind) {
	switch t {
	case themeLight:
		userStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#2a8a3a")).Bold(true)
		agentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#3a4ec0")).Bold(true)
		toolStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#b85a00"))
		errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#c01a2a"))
		thinkBox = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#5a3a00")).
				Background(lipgloss.Color("#fde79a")).
				Padding(0, 1)
		tuiHeader = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#1a1b26")).
				Background(lipgloss.Color("#fde79a")).
				Padding(0, 2).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#b88a00")).
				Align(lipgloss.Center)
		inputBoxActive = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), true, false, false, false).
				BorderForeground(lipgloss.Color("#3a4ec0"))
		inputBoxInactive = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), true, false, false, false).
				BorderForeground(lipgloss.Color("#9aa0a6"))
	default: // themeDark —— 深蓝/深色背景下使用全亮前景 + 强对比
		userStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(cUser)).Bold(true)
		agentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(cAgent)).Bold(true)
		toolStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(cTool))
		errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(cErr))
		// 思考卡片：亮黄底深色字，确保在深蓝背景下有最高对比度。
		thinkBox = lipgloss.NewStyle().
				Foreground(lipgloss.Color(cChipFG)).
				Background(lipgloss.Color(cThink)).
				Bold(true).
				Padding(0, 1)
		tuiHeader = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color(cThink)).
				Background(lipgloss.Color(cPanelBG)).
				Padding(0, 2).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color(cThink)).
				Align(lipgloss.Center)
		inputBoxActive = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), true, false, false, false).
				BorderForeground(lipgloss.Color(cAgent))
		inputBoxInactive = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), true, false, false, false).
				BorderForeground(lipgloss.Color(cLine))
	}
	logStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(cMuted))
	infoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(cMuted))
}

// newMDRenderer 按主题创建 glamour markdown 渲染器，统一去掉 document 左边距。
// dark 主题使用定制高对比配置：所有正文/标题/列表/代码都用亮前景色；
// 浅色主题用 LightStyleConfig。
func newMDRenderer(t themeKind, w int) (*glamour.TermRenderer, error) {
	var cfg ansi.StyleConfig
	if t == themeLight {
		cfg = styles.LightStyleConfig
	} else {
		cfg = customDarkStyleConfig()
	}
	cfg.Document.Margin = uUint(0)
	return glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(w),
	)
}

// customDarkStyleConfig 为深色（含深蓝）终端定制的高对比 markdown 样式：
// 全部使用亮前景 + 强对比，避开 glamour 默认 dark 在彩色背景下显示为暗色的问题。
func customDarkStyleConfig() ansi.StyleConfig {
	ink := hexStr(cInk)
	think := hexStr(cThink)
	success := hexStr(cSuccess)
	user := hexStr(cUser)
	agent := hexStr(cAgent)
	codeFG := hexStr(cCodeFG)
	codeBG := hexStr(cCodeBG)
	muted := hexStr(cMuted)
	line := hexStr(cLine)
	bold := uBool(true)
	italic := uBool(true)
	indent1 := uUint(1)
	margin0 := uUint(0)

	return ansi.StyleConfig{
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color:           ink,
				BackgroundColor: nil, // 透明背景，依赖终端底色
			},
			Margin: margin0,
		},
		Paragraph: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: ink},
			Margin:         margin0,
		},
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: think, Bold: bold},
		},
		H1: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: think, Bold: bold, Prefix: "# "},
		},
		H2: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: success, Bold: bold, Prefix: "## "},
		},
		H3: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: user, Bold: bold, Prefix: "### "},
		},
		H4: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: agent, Bold: bold, Prefix: "#### "},
		},
		H5: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: agent, Bold: bold, Prefix: "##### "},
		},
		H6: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: agent, Bold: bold, Prefix: "###### "},
		},
		Text:           ansi.StylePrimitive{Color: ink},
		Strong:         ansi.StylePrimitive{Color: think, Bold: bold},
		Emph:           ansi.StylePrimitive{Color: ink, Italic: italic},
		Strikethrough:  ansi.StylePrimitive{Color: muted, CrossedOut: uBool(true)},
		HorizontalRule: ansi.StylePrimitive{Color: line, Format: "─"},
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: user},
			Indent:         uUint(2),
			IndentToken:    strPtr("▎ "),
		},
		List: ansi.StyleList{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{Color: ink},
				Margin:         margin0,
			},
			LevelIndent: 4,
		},
		Item:        ansi.StylePrimitive{Color: agent, BlockPrefix: "• "},
		Enumeration: ansi.StylePrimitive{Color: agent},
		Task:        ansi.StyleTask{StylePrimitive: ansi.StylePrimitive{Color: success}},
		Link:        ansi.StylePrimitive{Color: user, Underline: uBool(true)},
		LinkText:    ansi.StylePrimitive{Color: user, Bold: bold},
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: codeFG, BackgroundColor: codeBG},
		},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{Color: codeFG, BackgroundColor: codeBG},
				Margin:         margin0,
				Indent:         indent1,
			},
			Theme: "nord",
		},
		Table: ansi.StyleTable{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{Color: ink},
			},
		},
	}
}

func strPtr(s string) *string { return &s }
