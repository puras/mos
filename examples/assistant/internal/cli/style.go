package cli

import (
	"os"

	"github.com/charmbracelet/lipgloss"
)

// 终端样式：基于 lipgloss 封装常用着色与排版样式。
// 仅在标准输出为终端时启用；重定向/管道输出时自动降级为纯文本，避免日志文件混入颜色码。
var colorEnabled = func() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}()

var (
	boldStyle   = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Faint(true)
	redStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	greenStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	yellowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	cyanStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	// bannerStyle 启动横幅：加粗青色圆角边框，居中对齐。
	bannerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("14")).
			Padding(0, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("14")).
			Align(lipgloss.Center)
)

// paint 用 lipgloss 样式渲染文本；颜色禁用时原样返回。
func paint(s lipgloss.Style, text string) string {
	if !colorEnabled {
		return text
	}
	return s.Render(text)
}

// bold 加粗（标题/标识）。
func bold(s string) string { return paint(boldStyle, s) }

// dim 弱化（次要说明）。
func dim(s string) string { return paint(dimStyle, s) }

// red 红色（错误/失败）。
func red(s string) string { return paint(redStyle, s) }

// green 绿色（成功）。
func green(s string) string { return paint(greenStyle, s) }

// yellow 黄色（过程/用量）。
func yellow(s string) string { return paint(yellowStyle, s) }

// cyan 青色（标识/标题）。
func cyan(s string) string { return paint(cyanStyle, s) }

// banner 渲染带边框的标题横幅。
func banner(s string) string { return paint(bannerStyle, s) }
