package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/puras/mos/filesystem"
	"github.com/puras/mos/tool"
)

// GrantRoot 是「申请访问目录」工具：把一个目录加进**运行期可变的根集合**。
//
// 为什么需要它：会话进行到一半，用户会授权新目录（"B 目录你也可以看"）。
// 这个动作需要一个**可审计、可审批**的入口——不能靠模型在文件工具的参数里
// 悄悄把边界撑开（那等于让模型自己扩权）。
//
// 三条纪律：
//
//   - **它不自己判"能不能加"**：授权是**宿主的政策**。宿主应当用参数级规则
//     （policy.Check 的 ask 档）把它接到人工审批通道上——人点了同意，工具
//     才执行。没接审批就注册本工具 = 明确允许模型自己扩边界（宿主自己的
//     选择，框架不越界，但要说清楚）。
//   - **fail-closed**：根集合为 nil → 拒绝；路径不是绝对路径、目录不存在或
//     不是目录 → 拒绝。授权一个不存在的东西没有意义。
//   - **留痕**：调用本身落 `tool/call` + `tool/result`（谁请求的、批没批、
//     加了什么都在日志里）；根变更另记在 `RootSet.History()`。
//
// 生效时机：加根对**下一次工具调用**立即生效（边界每次调用现判）——所以
// 模型拿到本工具的成功回执后，可以直接 read_file 那个目录。
type GrantRoot struct {
	// Roots 是目标根集合（运行期可变的那一份）。
	Roots *filesystem.RootSet
}

// NewGrantRoot 构造"申请访问目录"工具。
func NewGrantRoot(roots *filesystem.RootSet) *GrantRoot { return &GrantRoot{Roots: roots} }

type grantParams struct {
	Path string `json:"path" mos:"desc=要授权的目录绝对路径"`
	Mode string `json:"mode,omitempty" mos:"desc=授权范围：read / write / both，默认 both"`
}

func (GrantRoot) Name() string { return "grant_access" }

func (GrantRoot) Description() string {
	return "申请访问一个额外目录（需人工确认后生效）。参数：path=目录的绝对路径，" +
		"mode=授权范围（read / write / both，默认 both）。" +
		"只有在用户明确授权你访问该目录时才调用；用户只是要求查看某个尚未授权的目录时不要调用。"
}

func (GrantRoot) Parameters() any { return &grantParams{} }

// Guideline 实现 tool.Guideline：把"什么时候能申请授权"讲清楚。
//
// 判断标准是"模型忘掉它会在下一次调用犯错"——模型很容易在用户说"看下 B 目录"
// 时就直接申请授权（而不是先说明 B 不在允许范围内），那等于把授权的判断权
// 从人手里拿走。
func (g GrantRoot) Guideline() string {
	if g.Roots == nil {
		return ""
	}
	return "访问范围当前是封闭的：只有已授权的目录能读/写，其他路径会被拒绝并告诉你允许的根。" +
		"只有当用户明确授权你访问某个目录（例如“你也可以看 X 目录”）时才调用 grant_access 申请它；" +
		"如果用户只是要求查看某个尚未授权的目录，不要自行申请——先向用户说明该目录不在允许范围内，" +
		"并请用户确认是否授权。"
}

// Execute 实现 tool.Tool.Execute。
func (g GrantRoot) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[grantParams](args)
	if err != nil {
		return nil, err
	}
	if g.Roots == nil {
		return nil, errors.New("未装配根集合（拒绝授权：没有根集合就没有可改的边界）")
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path 不能为空")
	}
	if !filepath.IsAbs(p.Path) {
		return nil, fmt.Errorf("path 必须是绝对路径（收到 %q）：授权的目录要能唯一确定", p.Path)
	}
	dir := filepath.Clean(p.Path)

	// 目录必须存在且确实是目录：授权一个不存在的路径没有意义（也可能是模型
	// 把路径写错了——那更该让它改对，而不是先扩边界）。
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("目录不可用 %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q 不是目录", dir)
	}

	mode := p.Mode
	if mode == "" {
		mode = "both"
	}
	var added []string
	switch mode {
	case "read":
		added = g.Roots.AddRead(dir)
	case "write":
		added = g.Roots.AddWrite(dir)
	case "both":
		added = g.Roots.Add(dir)
	default:
		return nil, fmt.Errorf("mode 只能是 read / write / both（收到 %q）", mode)
	}

	snap := g.Roots.Snapshot()
	note := "已授权，现在可以访问该目录"
	if len(added) == 0 {
		note = "该目录此前已在授权范围内（本次无变化）"
	}
	return map[string]any{
		"path":        dir,
		"mode":        mode,
		"added":       added,
		"read_roots":  len(snap.ReadRoots),
		"write_roots": len(snap.WriteRoots),
		"note":        note,
	}, nil
}

var _ tool.Tool = (*GrantRoot)(nil)
var _ tool.Guideline = (*GrantRoot)(nil)
