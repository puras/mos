// Package inmemory 提供基于进程内内存的会话记忆实现，适合单机与测试场景。
package inmemory

import (
	"context"
	"sync"

	"hegq.net/mos/pkg/llm"
)

// Store 是基于内存的 Memory 实现。
type Store struct {
	mu       sync.RWMutex
	sessions map[string][]llm.Message
}

// New 创建一个空的内存记忆存储。
func New() *Store {
	return &Store{sessions: make(map[string][]llm.Message)}
}

// Load 实现 memory.Memory。
func (s *Store) Load(_ context.Context, sessionID string) ([]llm.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]llm.Message(nil), s.sessions[sessionID]...), nil
}

// Append 实现 memory.Memory。
func (s *Store) Append(_ context.Context, sessionID string, msg llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = append(s.sessions[sessionID], msg)
	return nil
}

// Clear 实现 memory.Memory。
func (s *Store) Clear(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
	return nil
}
