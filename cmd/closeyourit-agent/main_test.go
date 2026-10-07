package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// CYAG-9: the metrics push and the action loop run on separate tickers. While an action stays
// blocked for a long time, the push must keep firing — with the old single loop the action held
// everything still and the machine looked down.
func TestSlowActionDoesNotStopTheMetricsPush(t *testing.T) {
	stop := make(chan struct{})
	blocked := make(chan struct{})
	defer close(stop)    // runs second: stops both loops
	defer close(blocked) // runs first: unblocks the hanging action

	var pushes int64
	pushTick := func() { atomic.AddInt64(&pushes, 1) }
	actionTick := func() { <-blocked } // hangs for the whole test

	go supervise(stop, time.Millisecond, pushTick, actionTick)

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt64(&pushes) < 5 {
		select {
		case <-deadline:
			t.Fatalf("push blocked by the slow action: only %d pushes in 2s", atomic.LoadInt64(&pushes))
		case <-time.After(time.Millisecond):
		}
	}
}

// CYAG-9: at startup the action loop must run a round right away (a pending outbox and queued
// actions must not wait a whole interval). The long interval ensures the test passes only thanks
// to the immediate first round, not the ticker.
func TestActionsStartImmediatelyWithoutWaitingAnInterval(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)

	fired := make(chan struct{}, 1)
	actionTick := func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	}

	go supervise(stop, time.Hour, func() {}, actionTick)

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the action loop did not start right away at startup: it waited an interval")
	}
}

// loopUntil must stop when stop is closed, without leaving hanging goroutines.
func TestLoopUntilStopsWhenStopIsClosed(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})
	var ticks int64

	go func() {
		loopUntil(stop, time.Millisecond, func() { atomic.AddInt64(&ticks, 1) })
		close(done)
	}()

	// wait for at least one tick, then stop
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt64(&ticks) == 0 {
		select {
		case <-deadline:
			t.Fatal("loopUntil never ran fn")
		case <-time.After(time.Millisecond):
		}
	}
	close(stop)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loopUntil did not return after stop was closed")
	}
}

// CYAG-19: the server rejected the per-machine credential (R401-SERVER-002). The agent must drop
// it and go back to the enrollment token, so the next tick re-enrolls on its own: before, it stayed
// on the dead token and the only recovery was logging into the host to delete credentials.json.
type fakeStore struct {
	cleared bool
	err     error
}

func (s *fakeStore) Clear() error { s.cleared = true; return s.err }

type fakePusher struct{ token string }

func (p *fakePusher) SetToken(token string) { p.token = token }

func TestReenrollmentDropsTheCredentialAndReturnsToTheEnrollmentToken(t *testing.T) {
	store := &fakeStore{}
	push := &fakePusher{token: "cyi_h_dead"}

	if err := reenroll(store, push, "cyi_s_fleet"); err != nil {
		t.Fatalf("reenroll: %v", err)
	}
	if !store.cleared {
		t.Error("the rejected per-machine credential was not deleted")
	}
	if push.token != "cyi_s_fleet" {
		t.Errorf("bearer = %q, expected the enrollment token", push.token)
	}
}

// If the file cannot be deleted, the bearer does not change: otherwise on restart the agent would
// reload the dead credential from disk and go mute again without anyone noticing.
func TestReenrollmentKeepsBearerWhenDeletionFails(t *testing.T) {
	store := &fakeStore{err: errors.New("read-only disk")}
	push := &fakePusher{token: "cyi_h_dead"}

	if err := reenroll(store, push, "cyi_s_fleet"); err == nil {
		t.Fatal("expected an error")
	}
	if push.token != "cyi_h_dead" {
		t.Errorf("bearer changed despite the failed deletion: %q", push.token)
	}
}
