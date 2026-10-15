package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Persistence 是会话日志持久化后端接口（backend 可换）。
// 数据库（SQLite / Postgres）等重后端由业务层实现并注入，SDK 不内置。
type Persistence interface {
	SaveHeader(id string, h Header) error
	Append(id string, ev Event) error
	Load(id string) ([]Event, error)
	Close() error
}

// Header 是日志文件首行的会话元信息。
type Header struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
}

// JSONL 是轻量默认后端：每会话一个 .jsonl 文件，
// 首行 Header、逐行事件追加写；容忍尾部半行（崩溃时最后一条写一半）。
type JSONL struct {
	dir string
}

// NewJSONL 创建 JSONL 后端，dir 不存在时自动创建。
func NewJSONL(dir string) (*JSONL, error) {
	if dir == "" {
		return nil, fmt.Errorf("session: JSONL 目录为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: 创建日志目录: %w", err)
	}
	return &JSONL{dir: dir}, nil
}

func (j *JSONL) path(id string) string {
	return filepath.Join(j.dir, id+".jsonl")
}

// SaveHeader 写入日志首行元信息；文件已存在时不重复写。
func (j *JSONL) SaveHeader(id string, h Header) error {
	p := j.path(id)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// Append 追加一行事件并落盘。写失败返回错误，由 Session.Append 落实 fail-fast。
func (j *JSONL) Append(id string, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(j.path(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Load 读取事件（跳过首行 Header）。
// 尾部半行（文件不以换行结尾的最后一段，崩溃残留）丢弃；
// 其余行损坏返回错误——日志中段损坏属于异常，不允许静默跳过。
func (j *JSONL) Load(id string) ([]Event, error) {
	data, err := os.ReadFile(j.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	if last := lines[len(lines)-1]; last != "" {
		// 文件不以换行结尾：最后一段是崩溃残留的半行，丢弃
		lines = lines[:len(lines)-1]
	}

	var events []Event
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i == 0 { // Header
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("session: 日志第 %d 行损坏 %s: %w", i+1, id, err)
		}
		events = append(events, ev)
	}
	return events, nil
}

// Close 释放资源（JSONL 无持有资源，恒为 nil）。
func (j *JSONL) Close() error { return nil }

// Restore 从后端重放事件重建会话，并修复孤儿 turn：
// 日志尾部的 turn/start 没有配对 turn/end 时（崩溃残留），在内存中合成
// turn/end{reason: interrupted} 平衡日志——只补事件，不回写已有日志。
// 会话不存在时返回空会话（不报错）；后续 Append 照常持久化。
func Restore(id string, p Persistence) (*Session, error) {
	events, err := p.Load(id)
	if err != nil {
		return nil, err
	}
	s := New(id, p)
	for _, ev := range events {
		s.seq = ev.Seq
		s.events = append(s.events, ev)
	}
	if s.repairOrphanTurn() {
		ev := Event{Seq: s.seq + 1, Kind: EventTurnEnd, Reason: "interrupted", Time: time.Now()}
		s.seq = ev.Seq
		s.events = append(s.events, ev)
	}
	return s, nil
}

// repairOrphanTurn 检查日志尾部是否有未配对的 turn/start：从尾部向前，
// 先遇到 turn/end 说明最后一轮完整；先遇到 turn/start 说明孤儿。
func (s *Session) repairOrphanTurn() bool {
	for i := len(s.events) - 1; i >= 0; i-- {
		switch s.events[i].Kind {
		case EventTurnEnd:
			return false
		case EventTurnStart:
			return true
		}
	}
	return false
}
