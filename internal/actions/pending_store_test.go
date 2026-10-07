package actions

import "testing"

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
