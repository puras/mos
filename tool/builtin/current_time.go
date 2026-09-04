package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/puras/mos/tool"
)

// CurrentTime 返回当前日期时间，可指定时区。
type CurrentTime struct{}

type timeParams struct {
	Timezone string `json:"timezone,omitempty" mos:"desc=IANA 时区标识，如 Asia/Shanghai、UTC，留空使用本地时区"`
}

func (CurrentTime) Name() string { return "current_time" }
func (CurrentTime) Description() string {
	return "获取当前日期与时间，支持指定 IANA 时区"
}
func (CurrentTime) Parameters() any { return &timeParams{} }
func (CurrentTime) Execute(_ context.Context, args json.RawMessage) (any, error) {
	p, err := tool.Parse[timeParams](args)
	if err != nil {
		return nil, err
	}
	loc := time.Local
	if p.Timezone != "" {
		loc, err = time.LoadLocation(p.Timezone)
		if err != nil {
			return nil, fmt.Errorf("无效时区 %q: %w", p.Timezone, err)
		}
	}
	now := time.Now().In(loc)
	return map[string]any{
		"time":     now.Format("2006-01-02 15:04:05"),
		"weekday":  now.Weekday().String(),
		"timezone": loc.String(),
		"unix":     now.Unix(),
		"iso8601":  now.Format(time.RFC3339),
	}, nil
}

var _ tool.Tool = CurrentTime{}
