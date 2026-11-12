package actions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPendingStoreKeepsStartNote(t *testing.T) {
	store := NewPendingStore(t.TempDir())
	if err := store.Save(Pending{ActionID: "a1", Kind: "reboot", Started: true}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got == nil || !got.Started || got.ActionID != "a1" || got.Kind != "reboot" || got.Result.Status != "" {
		t.Fatalf("start note not kept: got=%+v err=%v", got, err)
	}
}

// The result replaces the start note in a single atomic write: no note left behind.
func TestPendingStoreResultReplacesStartNote(t *testing.T) {
	dir := t.TempDir()
	store := NewPendingStore(dir)
	if err := store.Save(Pending{ActionID: "a1", Started: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Pending{ActionID: "a1", Result: Result{Status: "succeeded"}}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got == nil || got.Started || got.Result.Status != "succeeded" {
		t.Fatalf("result did not replace the start note: got=%+v err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pending-action-result.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

// A file written by an older agent (no "started" key) keeps loading as a finished result.
func TestPendingStoreLoadsOldFormatAsFinishedResult(t *testing.T) {
	dir := t.TempDir()
	old := `{"action_id":"a_old","result":{"status":"succeeded","exit_code":0,"output":"ok"}}`
	if err := os.WriteFile(filepath.Join(dir, "pending-action-result.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := NewPendingStore(dir).Load()
	if err != nil || got == nil || got.Started || got.ActionID != "a_old" || got.Result.Status != "succeeded" || got.Result.Output != "ok" {
		t.Fatalf("old format not loaded as a finished result: got=%+v err=%v", got, err)
	}
}

func TestPendingStoreSaveFailsOnUnwritableDir(t *testing.T) {
	store := NewPendingStore(filepath.Join(t.TempDir(), "missing"))
	if err := store.Save(Pending{ActionID: "a1", Started: true}); err == nil {
		t.Fatal("expected an error writing into a missing directory")
	}
}

func TestPendingStoreSurvivesRestart(t *testing.T) {
	store := NewPendingStore(t.TempDir())
	want := Pending{ActionID: "a1", Result: Result{Status: "succeeded", ExitCode: 0}}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got == nil || got.ActionID != want.ActionID {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load()
	if err != nil || got != nil {
		t.Fatalf("after clear got=%+v err=%v", got, err)
	}
}
