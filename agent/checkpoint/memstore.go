package checkpoint

import (
	"context"
	"errors"
	"sync"
)

// MemStore 是 Store 的内存实现：sessionID → 最新 checkpoint（单测与本地调试用）。
// 并发安全；Delete 清空指定会话。
type MemStore struct {
	mu      sync.Mutex
	entries map[string]*Checkpoint
}

// NewMemStore 创建空 Store。
func NewMemStore() *MemStore {
	return &MemStore{entries: make(map[string]*Checkpoint)}
}

// Save 写入（同 sessionID 覆盖先写）。
func (m *MemStore) Save(ctx context.Context, ckpt *Checkpoint) error {
	if m == nil || ckpt == nil {
		return errors.New("checkpoint: nil store or checkpoint")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *ckpt
	m.entries[ckpt.SessionID] = &cp
	return nil
}

// Load 读取指定会话的最新 checkpoint。
func (m *MemStore) Load(ctx context.Context, sessionID string) (*Checkpoint, error) {
	if m == nil {
		return nil, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ckpt, ok := m.entries[sessionID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *ckpt
	return &cp, nil
}

// Delete 清空指定会话。
func (m *MemStore) Delete(ctx context.Context, sessionID string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, sessionID)
	return nil
}
