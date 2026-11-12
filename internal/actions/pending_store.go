package actions

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Pending is the single entry of the action outbox on disk. It holds either a start note
// (Started true, no result yet: the command was about to run or still running) or the result
// still to report. A file written before the start note existed has no "started" key, so it
// keeps loading as a finished result.
type Pending struct {
	ActionID string `json:"action_id"`
	Kind     string `json:"kind,omitempty"`
	Started  bool   `json:"started,omitempty"`
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

// Save replaces the outbox atomically (temporary file + rename): the result written after the
// command takes the place of the start note in a single step, with no state in between.
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
