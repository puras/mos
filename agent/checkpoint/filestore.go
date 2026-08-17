package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileStore 是 Store 的文件实现（默认后端）：每会话单文件
// `<dir>/<sessionID>.checkpoint.json`。写入走 tmp + Rename 原子化
// （并发/崩溃下不产生半写文件）。数据库后端由业务层实现 Store 注入。
type FileStore struct {
	dir string
}

// NewFileStore 创建以 dir 为根目录的 Store（目录不存在时懒创建）。
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("checkpoint: file store requires dir")
	}
	return &FileStore{dir: dir}, nil
}

// path 返回指定会话的 checkpoint 文件路径。
func (f *FileStore) path(sessionID string) string {
	return filepath.Join(f.dir, sessionID+".checkpoint.json")
}

// Save 原子写入：tmp 文件 + Rename。
func (f *FileStore) Save(ctx context.Context, ckpt *Checkpoint) error {
	if ckpt == nil {
		return errors.New("checkpoint: nil checkpoint")
	}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return fmt.Errorf("checkpoint: mkdir: %w", err)
	}
	b, err := json.MarshalIndent(ckpt, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: marshal: %w", err)
	}
	path := f.path(ckpt.SessionID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("checkpoint: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("checkpoint: rename: %w", err)
	}
	return nil
}

// Load 读取指定会话的 checkpoint；无文件返回 ErrNotFound。
func (f *FileStore) Load(ctx context.Context, sessionID string) (*Checkpoint, error) {
	b, err := os.ReadFile(f.path(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("checkpoint: read: %w", err)
	}
	var ckpt Checkpoint
	if err := json.Unmarshal(b, &ckpt); err != nil {
		return nil, fmt.Errorf("checkpoint: unmarshal: %w", err)
	}
	return &ckpt, nil
}

// Delete 删除指定会话的 checkpoint 文件。
func (f *FileStore) Delete(ctx context.Context, sessionID string) error {
	err := os.Remove(f.path(sessionID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("checkpoint: remove: %w", err)
	}
	return nil
}
