package credentials

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Store struct{ path string }
type data struct {
	HostToken string `json:"host_token"`
}

func New(dataDir string) Store { return Store{path: filepath.Join(dataDir, "credentials.json")} }

func (s Store) Load() (string, error) {
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var value data
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value.HostToken, nil
}

func (s Store) Save(token string) error {
	raw, err := json.Marshal(data{HostToken: token})
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

// Clear removes the per-machine credential. It is needed when the server rejects it
// (401 R401-SERVER-002): the agent must be able to go back to the enrollment token on its own,
// without someone logging into the host to delete the file by hand (CYAG-19). A missing file is
// not an error.
func (s Store) Clear() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
