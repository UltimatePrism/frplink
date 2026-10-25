package store

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings 全局设置（存储在数据目录 settings.json）。
type Settings struct {
	// ConnectTimeoutSec 启动后持续"连接中/重试中"超过该秒数视为超时并自动停止
	ConnectTimeoutSec int `json:"connectTimeoutSec"`
}

// DefaultSettings 默认设置。
func DefaultSettings() Settings {
	return Settings{ConnectTimeoutSec: 10}
}

func (s *Settings) normalize() {
	if s.ConnectTimeoutSec < 3 {
		s.ConnectTimeoutSec = 3
	}
	if s.ConnectTimeoutSec > 300 {
		s.ConnectTimeoutSec = 300
	}
}

func (s *Store) settingsPath() string {
	return filepath.Join(filepath.Dir(s.path), "settings.json")
}

// Settings 读取设置；文件缺失或损坏时返回默认值。
func (s *Store) Settings() Settings {
	out := DefaultSettings()
	data, err := os.ReadFile(s.settingsPath())
	if err != nil {
		return out
	}
	var v Settings
	if json.Unmarshal(data, &v) != nil {
		return out
	}
	v.normalize()
	return v
}

// SaveSettings 保存设置（原子写盘）。
func (s *Store) SaveSettings(v Settings) error {
	v.normalize()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.settingsPath()), 0o755); err != nil {
		return err
	}
	tmp := s.settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.settingsPath())
}
