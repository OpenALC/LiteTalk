package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Delivery 去重记录：已成功处理过的 webhook 投递。
type Delivery struct {
	ID  string    `json:"id"`
	Tag string    `json:"tag"`
	At  time.Time `json:"at"`
}

// State 更新器持久化状态（state.json）。
// 只记 tag/校验和/耗时等元数据，绝不落 secret 与请求体。
type State struct {
	CurrentVersion string     `json:"current_version"` // 最近一次成功装上的 tag
	LastTag        string     `json:"last_tag"`
	LastResult     string     `json:"last_result"` // success | failed | ""
	LastError      string     `json:"last_error,omitempty"`
	LastDelivery   string     `json:"last_delivery,omitempty"`
	LastChecksum   string     `json:"last_checksum,omitempty"`
	LastDurationMs int64      `json:"last_duration_ms,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at,omitempty"`
	Deliveries     []Delivery `json:"deliveries,omitempty"`
}

// HasDelivery 该 delivery 是否已成功处理过（去重）。
func (s State) HasDelivery(id string) bool {
	if id == "" {
		return false
	}
	for _, d := range s.Deliveries {
		if d.ID == id {
			return true
		}
	}
	return false
}

// StateStore 状态文件读写。单进程内用互斥锁；落盘走 tmp+rename 原子替换。
type StateStore struct {
	dir  string
	path string
	mu   sync.Mutex
	st   State
	now  func() time.Time
}

// OpenState 打开（或初始化）状态目录。目录 0700 创建：状态含部署元数据，不给他人读。
func OpenState(dir string) (*StateStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("建状态目录: %w", err)
	}
	s := &StateStore{dir: dir, path: filepath.Join(dir, "state.json"), now: time.Now}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", s.path, err)
		}
		return s, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读 %s: %w", s.path, err)
	}
	return s, nil
}

// Get 返回状态快照。
func (s *StateStore) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Deliveries = append([]Delivery(nil), s.st.Deliveries...)
	return s.st
}

// Update 在锁内变更状态并原子落盘。fn 抛错时不落盘。
func (s *StateStore) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
	// delivery 记录环形淘汰，防 state.json 无界增长
	if len(s.st.Deliveries) > DeliveryKeep {
		s.st.Deliveries = s.st.Deliveries[len(s.st.Deliveries)-DeliveryKeep:]
	}
	b, err := json.MarshalIndent(&s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// RecordSuccess 成功：推进当前版本并记下 delivery。
func (s *StateStore) RecordSuccess(tag, delivery, checksum string, dur time.Duration) error {
	return s.Update(func(st *State) {
		st.CurrentVersion = tag
		st.LastTag = tag
		st.LastResult = "success"
		st.LastError = ""
		st.LastDelivery = delivery
		st.LastChecksum = checksum
		st.LastDurationMs = dur.Milliseconds()
		st.UpdatedAt = s.now()
		if delivery != "" {
			st.Deliveries = append(st.Deliveries, Delivery{ID: delivery, Tag: tag, At: s.now()})
		}
	})
}

// RecordFailure 失败：不记 delivery（允许 GitHub 重投/人工 redeliver 重试），只留失败原因。
func (s *StateStore) RecordFailure(tag, delivery string, cause error, dur time.Duration) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	return s.Update(func(st *State) {
		st.LastTag = tag
		st.LastResult = "failed"
		st.LastError = msg
		st.LastDelivery = delivery
		st.LastDurationMs = dur.Milliseconds()
		st.UpdatedAt = s.now()
	})
}
