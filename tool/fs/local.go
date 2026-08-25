package fs

import (
	"context"
	"path/filepath"
)

// Local 是 FileSystem 的透传实现（仅 ModeDangerFull 使用）。
// 不做任何 jail 校验；路径原样返回。
type Local struct {
	mode Mode
}

// NewLocal 构造透传实现的 FileSystem（mode 通常为 ModeDangerFull）。
// mode 字段未导出，跨包构造必须经此入口。
func NewLocal(mode Mode) *Local {
	return &Local{mode: mode}
}

var _ FileSystem = (*Local)(nil)

// Resolve 实现 FileSystem.Resolve：filepath.Abs 后直接返回。
func (l *Local) Resolve(ctx context.Context, req Request) (*Resolved, error) {
	abs, err := filepath.Abs(req.Path)
	if err != nil {
		return nil, err
	}
	return &Resolved{
		DisplayPath: abs,
		TargetKey:   abs,
	}, nil
}

// Mode 实现 FileSystem.Mode。
func (l *Local) Mode() Mode { return l.mode }
