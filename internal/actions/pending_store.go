package actions

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Pending struct {
	ActionID string `json:"action_id"`
	Result   Result `json:"result"`
}

type PendingStore struct{ path string }

func NewPendingStore(dataDir string) PendingStore {
	return PendingStore{path: filepath.Join(dataDir, "pending-action-result.json")}
}

func (s PendingStore) Load() (*Pending, error) {
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending Pending
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, err
	}
	return &pending, nil
}

func (s PendingStore) Save(p Pending) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s PendingStore) Clear() error {
	err := os.Remove(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
