package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// JSONL 是 Persistence 的默认实现（架构 v2 §10.3 backend 可换的 jsonl 后端）：
// 每个会话一个文件 <dir>/<sessionID>.jsonl，首行 header，后续每行一条事件（追加写）。
// 事件顺序即磁盘顺序（Seq 连续），崩溃时尾部半行会被 Load 跳过（容忍截断写）。
type JSONL struct {
	dir string
	mu  sync.Mutex
}

// jsonlLine 是文件内一行的统一结构：header 行或事件行二选一。
type jsonlLine struct {
	Header *SessionHeader `json:"header,omitempty"`
	Event  *SessionEvent  `json:"event,omitempty"`
}

// NewJSONL 创建 JSONL 后端（目录不存在时创建）。
func NewJSONL(dir string) (*JSONL, error) {
	if dir == "" {
		return nil, fmt.Errorf("agent: jsonl: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("agent: jsonl: mkdir: %w", err)
	}
	return &JSONL{dir: dir}, nil
}

// SaveHeader 写入会话头部（首行；已存在则覆盖）。
func (j *JSONL) SaveHeader(sessionID string, h SessionHeader) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	path := j.path(sessionID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("agent: jsonl: open header: %w", err)
	}
	defer f.Close()
	line := jsonlLine{Header: &h}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("agent: jsonl: marshal header: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("agent: jsonl: write header: %w", err)
	}
	return nil
}

// LoadHeader 读取会话头部（首行）。会话不存在或文件无 header 行时返回 ErrNotFound。
func (j *JSONL) LoadHeader(sessionID string) (SessionHeader, error) {
	events, header, err := j.read(sessionID)
	if err != nil {
		return SessionHeader{}, err
	}
	_ = events
	if header == nil {
		return SessionHeader{}, ErrNotFound
	}
	return *header, nil
}

// Append 追加一条事件（文件不存在时视为空会话，仅追加事件行）。
func (j *JSONL) Append(sessionID string, ev SessionEvent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(j.path(sessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("agent: jsonl: open: %w", err)
	}
	defer f.Close()
	line := jsonlLine{Event: &ev}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("agent: jsonl: marshal event: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("agent: jsonl: write event: %w", err)
	}
	return nil
}

// Load 读取会话全部事件（按 Seq 升序；跳过头部行与损坏半行）。
func (j *JSONL) Load(sessionID string) ([]SessionEvent, error) {
	events, _, err := j.read(sessionID)
	return events, err
}

// SaveState 持久化任务状态到独立文件 <dir>/<sessionID>.state.json（M2.8）。
// 原子写：tmp + Rename，崩溃不产生半写文件。
func (j *JSONL) SaveState(sessionID string, st TaskState) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("agent: jsonl: marshal state: %w", err)
	}
	path := j.statePath(sessionID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("agent: jsonl: write state tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("agent: jsonl: rename state: %w", err)
	}
	return nil
}

// LoadState 读取任务状态；无记录返回 ErrNotFound。
func (j *JSONL) LoadState(sessionID string) (TaskState, error) {
	b, err := os.ReadFile(j.statePath(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return TaskState{}, ErrNotFound
		}
		return TaskState{}, fmt.Errorf("agent: jsonl: read state: %w", err)
	}
	var st TaskState
	if err := json.Unmarshal(b, &st); err != nil {
		return TaskState{}, fmt.Errorf("agent: jsonl: unmarshal state: %w", err)
	}
	return st, nil
}

// Close 实现 Persistence（JSONL 无长期资源，no-op）。
func (j *JSONL) Close() error { return nil }

func (j *JSONL) path(sessionID string) string {
	return filepath.Join(j.dir, sessionID+".jsonl")
}

func (j *JSONL) statePath(sessionID string) string {
	return filepath.Join(j.dir, sessionID+".state.json")
}

// read 读取文件：返回事件列表与头部（文件不存在视为空会话）。
func (j *JSONL) read(sessionID string) ([]SessionEvent, *SessionHeader, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.Open(j.path(sessionID))
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("agent: jsonl: open: %w", err)
	}
	defer f.Close()

	var events []SessionEvent
	var header *SessionHeader
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var l jsonlLine
		if err := json.Unmarshal(line, &l); err != nil {
			continue // 容忍损坏半行（崩溃截断写）
		}
		switch {
		case l.Header != nil:
			h := *l.Header
			header = &h
		case l.Event != nil:
			events = append(events, *l.Event)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("agent: jsonl: scan: %w", err)
	}
	return events, header, nil
}
