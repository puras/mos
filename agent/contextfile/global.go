package contextfile

import (
	_ "embed"
	"os"
	"path/filepath"
)

// globalTemplate 是 MoS 全局约定（~/.mos/AGENTS.md）的模板文本。
//
// 它是**模型侧**的通用约定：回答风格、做事的方式、边界（哪些是人的决定）。
// 每个会话都会加载它——所以要短（常驻开销）。
//
//go:embed templates/global-agents.md
var globalTemplate string

// GlobalTemplateFileName 是全局约定文件名（放在全局目录里）。
const GlobalTemplateFileName = "AGENTS.md"

// GlobalTemplate 返回 MoS 全局约定的模板文本。
//
// **纯数据**：本包不碰文件系统（ARCHITECTURE.md §4 治理规则 5）——落盘归宿主。
// 宿主拿到它之后：全局目录里还没有这个文件就写一份（**只写不覆盖**：用户改过
// 的版本永远保留），有就别动。
//
// 写完的约定文件会被每个会话加载（连同项目自己的 AGENTS.md）：全局的更一般，
// 项目的更具体，冲突时以项目为准。
func GlobalTemplate() string {
	return globalTemplate
}

// EnsureGlobalFile 在 dir 里写一份全局约定模板：**已存在就不覆盖**（返回
// created=false）。
//
// 这是**宿主侧**的便利封装：它写的是宿主自己的用户级目录（~/.mos），
// 不是模型能碰的路径；模板来自 SDK（contextfile.GlobalTemplate）。
//
// 为什么不自动覆盖：用户改过的约定是他们的选择——工具替他们重置，是越界。
// 为什么不每次都写：多一次无谓的磁盘写入，还会让「文件存在」失去意义。
//
// 目录不存在会建（~/.mos 本来就是 MoS 自己的目录）。
func EnsureGlobalFile(dir string) (created bool, path string, err error) {
	if dir == "" {
		return false, "", os.ErrInvalid
	}
	path = filepath.Join(dir, GlobalTemplateFileName)
	if _, err := os.Stat(path); err == nil {
		return false, path, nil // 已有：不动（用户改过的是他们的）
	} else if !os.IsNotExist(err) {
		return false, path, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, path, err
	}
	if err := os.WriteFile(path, []byte(GlobalTemplate()), 0o644); err != nil {
		return false, path, err
	}
	return true, path, nil
}
