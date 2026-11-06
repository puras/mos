package mcp

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	// MaxToolNameLength 是公开工具名的长度上限（64）。
	//
	// 取 64 的理由：PI / DSH 都用 64（模型侧工具名的常见上限）；名字太长
	// 既费 token 又容易撞供应商的名字长度限制。
	MaxToolNameLength = 64
	// hashSuffixLength 是超长时追加的哈希后缀长度（8 位十六进制）。
	//
	// PI 用 8 位、DSH 用 12 位——MoS 取 PI 的 8 位：名字短一点更省 token，
	// 而冲突概率在这一档工具规模下可忽略（32 位 ≈ 6.5 万个名字才撞一次，
	// 撞了也只是同名工具注册失败——Replace 整批拒绝、保留上一代，不会静默错配）。
	hashSuffixLength = 8
	// namePrefix 是所有 MCP 工具的命名空间前缀。
	namePrefix = "mcp__"
)

// ToolName 拼装公开工具名：mcp__<server>__<tool>。
//
// 规则（对齐 PI / DSH 的命名空间约定）：
//   - 非 [A-Za-z0-9_] 的字符一律换成 _（DSH 允许 -，PI 只允许 _——
//     MoS 取 PI 的更严规则：工具名还要进模型的 function name 段，
//     字符集越窄越安全）；
//   - 超长（> 64）时截断并追加 8 位 sha256 后缀（截断后的名字仍要能
//     区分不同工具——后缀由**完整原名**算出）。
func ToolName(server, tool string) string {
	name := namePrefix + sanitizeName(server) + "__" + sanitizeName(tool)
	if len(name) <= MaxToolNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:hashSuffixLength]
	keep := MaxToolNameLength - hashSuffixLength - 1
	return name[:keep] + "_" + suffix
}

// sanitizeName 把名字里的非法字符换成 _。
//
// 非 ASCII 字符按字节处理（每个字节换成一个 _）——名字是标识符，不是文本，
// 一个汉字变几个下划线无所谓，只要稳定（同名每次算出同一个结果）。
func sanitizeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
