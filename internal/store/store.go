// Package store 负责把服务器条目持久化到本地 JSON 文件。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"frplink/internal/model"
)

// Store 多服务器配置存储。线程安全。
type Store struct {
	mu      sync.Mutex
	path    string
	entries map[string]*model.ServerEntry
}

// DefaultDataDir 返回默认数据目录（<用户配置目录>/frplink）。
func DefaultDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "frplink"), nil
}

// DefaultPath 默认存储文件路径。
func DefaultPath() (string, error) {
	dir, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "servers.json"), nil
}

// Open 打开（或创建）存储。path 为空则使用默认路径。
func Open(path string) (*Store, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, fmt.Errorf("无法确定配置目录: %w", err)
		}
	}
	s := &Store{path: path, entries: map[string]*model.ServerEntry{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	var list []*model.ServerEntry
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("配置文件损坏(%s): %w", path, err)
	}
	for _, e := range list {
		if e.ID == "" {
			e.ID = NewID()
		}
		s.entries[e.ID] = e
	}
	return s, nil
}

// Path 返回存储文件路径。
func (s *Store) Path() string { return s.path }

// NewID 生成短随机 ID。
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// List 返回全部条目：优先按 Sort（拖拽排序），Sort 相同再按创建时间。
func (s *Store) List() []*model.ServerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.ServerEntry, 0, len(s.entries))
	for _, e := range s.entries {
		cp := *e
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get 返回条目副本；不存在返回 nil。
func (s *Store) Get(id string) *model.ServerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[id]; ok {
		cp := *e
		return &cp
	}
	return nil
}

// Create 新增条目（忽略传入的 ID/时间戳）。
func (s *Store) Create(e *model.ServerEntry) (*model.ServerEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ne := *e.Normalized()
	if err := ne.Validate(); err != nil {
		return nil, err
	}
	ne.ID = NewID()
	now := time.Now()
	ne.CreatedAt, ne.UpdatedAt = now, now
	// 新条目排在列表末尾
	ne.Sort = s.nextSortLocked()
	s.entries[ne.ID] = &ne
	if err := s.saveLocked(); err != nil {
		delete(s.entries, ne.ID)
		return nil, err
	}
	out := ne
	return &out, nil
}

// nextSortLocked 返回当前最大的 Sort + 1（需持有锁）。
func (s *Store) nextSortLocked() int {
	max := 0
	for _, e := range s.entries {
		if e.Sort > max {
			max = e.Sort
		}
	}
	return max + 1
}

// SetOrder 按 ids 顺序写入排序权重（拖拽排序后调用）。
func (s *Store) SetOrder(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	old := map[string]*model.ServerEntry{}
	for id, e := range s.entries {
		old[id] = e
	}
	for i, id := range ids {
		if e, ok := s.entries[id]; ok {
			ne := *e
			ne.Sort = i + 1
			s.entries[id] = &ne
		}
	}
	if err := s.saveLocked(); err != nil {
		s.entries = old
		return err
	}
	return nil
}

// Update 更新已有条目（以传入条目为准，保留创建时间）。
func (s *Store) Update(e *model.ServerEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.entries[e.ID]
	if !ok {
		return fmt.Errorf("条目不存在")
	}
	ne := *e.Normalized()
	if err := ne.Validate(); err != nil {
		return err
	}
	ne.CreatedAt = old.CreatedAt
	ne.UpdatedAt = time.Now()
	s.entries[ne.ID] = &ne
	if err := s.saveLocked(); err != nil {
		s.entries[ne.ID] = old
		return err
	}
	return nil
}

// SetTraffic 原子更新某个条目的累计流量（不影响其他字段）。
func (s *Store) SetTraffic(id string, traffic int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return nil
	}
	if e.Traffic == traffic {
		return nil
	}
	ne := *e
	ne.Traffic = traffic
	s.entries[id] = &ne
	return s.saveLocked()
}

// Delete 删除条目。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.entries[id]
	if !ok {
		return fmt.Errorf("条目不存在")
	}
	delete(s.entries, id)
	if err := s.saveLocked(); err != nil {
		s.entries[id] = old
		return err
	}
	return nil
}

// saveLocked 原子写盘（需持有锁）。
func (s *Store) saveLocked() error {
	list := make([]*model.ServerEntry, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, e)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
