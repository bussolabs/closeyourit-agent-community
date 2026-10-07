package actions

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeRunner struct {
	name string
	args []string
	out  []byte
	err  error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.name, f.args = name, args
	return f.out, f.err
}

func TestSecurityUpdatesUsesFixedCommand(t *testing.T) {
	r := &fakeRunner{out: []byte("ok")}
	result := NewExecutorWithRunner(r).Execute(Action{Kind: "apply_security_updates"})
	if result.Status != "succeeded" || r.name != "unattended-upgrade" || !reflect.DeepEqual(r.args, []string{"--verbose"}) {
		t.Fatalf("result=%+v command=%s %#v", result, r.name, r.args)
	}
}

// apply_all_updates is the only action that also touches non-security origins: the exact command
// is part of the contract, a typo here would upgrade the machine differently from what is declared.
func TestAllUpdatesUsesNonInteractiveAptUpgrade(t *testing.T) {
	r := &fakeRunner{out: []byte("ok")}
	result := NewExecutorWithRunner(r).Execute(Action{Kind: "apply_all_updates"})

	want := []string{"DEBIAN_FRONTEND=noninteractive", "apt-get", "-y", "-o", "Dpkg::Options::=--force-confold", "upgrade"}
	if result.Status != "succeeded" || r.name != "env" || !reflect.DeepEqual(r.args, want) {
		t.Fatalf("result=%+v command=%s %#v", result, r.name, r.args)
	}
}

// dist-upgrade would install/remove packages and pull in kernels: outside the action's mandate,
// which only upgrades what is already installed.
func TestAllUpdatesNeverUsesDistUpgrade(t *testing.T) {
	r := &fakeRunner{out: []byte("ok")}
	NewExecutorWithRunner(r).Execute(Action{Kind: "apply_all_updates"})

	for _, arg := range r.args {
		if arg == "dist-upgrade" || arg == "full-upgrade" {
			t.Fatalf("command too invasive: %#v", r.args)
		}
	}
}

func TestUnknownActionNeverRuns(t *testing.T) {
	r := &fakeRunner{}
	result := NewExecutorWithRunner(r).Execute(Action{Kind: "shell"})
	if result.Status != "failed" || r.name != "" {
		t.Fatalf("result=%+v command=%s", result, r.name)
	}
}

func TestFailureIsReportedAndOutputCapped(t *testing.T) {
	r := &fakeRunner{out: make([]byte, maxOutput+1), err: errors.New("boom")}
	result := NewExecutorWithRunner(r).Execute(Action{Kind: "apply_security_updates"})
	if result.Status != "failed" || len(result.Output) != maxOutput {
		t.Fatalf("result=%+v", result)
	}
}
