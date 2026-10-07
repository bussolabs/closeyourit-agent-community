package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Save("cyi_h_secret"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got != "cyi_h_secret" {
		t.Fatalf("got %q, err %v", got, err)
	}
	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestStoreMissingIsEmpty(t *testing.T) {
	got, err := New(t.TempDir()).Load()
	if err != nil || got != "" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

// CYAG-19: when the server rejects the per-machine credential, the agent must be able to drop it on
// its own — before, someone had to log into the host and delete the file by hand.
func TestStoreClearRemovesTheCredential(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Save("cyi_h_secret"); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := store.Load()
	if err != nil || got != "" {
		t.Fatalf("after Clear: got %q, err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("credentials.json still present: %v", err)
	}
}

// Clear on a never-saved credential is not an error: the agent calls it without knowing whether the file exists.
func TestStoreClearWithoutFileIsNotAnError(t *testing.T) {
	if err := New(t.TempDir()).Clear(); err != nil {
		t.Fatalf("clear on an empty store: %v", err)
	}
}
