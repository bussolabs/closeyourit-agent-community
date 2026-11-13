package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bussolabs/closeyourit-agent/internal/pusher"
)

// The loop tests run inside a synctest bubble: time is simulated and only moves when every goroutine
// of the bubble is blocked, so time.Sleep in the test advances the clock explicitly and
// synctest.Wait lets the loops react before the assertions. No test waits on the real clock.

// CYAG-9: the metrics push and the action loop run on separate tickers. While an action stays
// blocked for a long time, the push must keep firing — with the old single loop the action held
// everything still and the machine looked down.
func TestSlowActionDoesNotStopTheMetricsPush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stop := make(chan struct{})
		blocked := make(chan struct{})
		defer close(stop)    // runs second: stops both loops
		defer close(blocked) // runs first: unblocks the hanging action

		var pushes, actions atomic.Int64
		pushTick := func() { pushes.Add(1) }
		actionTick := func() { actions.Add(1); <-blocked } // hangs until the end of the test

		go supervise(stop, time.Minute, 0, pushTick, actionTick)

		time.Sleep(5*time.Minute + time.Second)
		synctest.Wait()
		// one push after the (zero) jitter, then one per interval: 0, 1, 2, 3, 4, 5 minutes
		if n := pushes.Load(); n != 6 {
			t.Fatalf("push blocked by the slow action: %d pushes in 5 minutes, expected 6", n)
		}
		if n := actions.Load(); n != 1 {
			t.Fatalf("action rounds = %d, expected the single one still hanging", n)
		}
	})
}

// CYAG-9: at startup the action loop must run a round right away (a pending outbox and queued
// actions must not wait a whole interval). The long interval and jitter ensure the test passes only
// thanks to the immediate first round, not the ticker.
func TestActionsStartImmediatelyWithoutWaitingAnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stop := make(chan struct{})
		defer close(stop)
		var actions atomic.Int64

		go supervise(stop, time.Hour, time.Hour, func() {}, func() { actions.Add(1) })

		synctest.Wait() // no simulated time has passed
		if n := actions.Load(); n != 1 {
			t.Fatalf("the action loop did not start right away at startup: %d rounds at time zero", n)
		}
	})
}

// CYAG-28: after a fleet-wide restart every host pushed on the same second of every minute. The
// regular cadence now starts after a random delay that never exceeds one interval.
func TestStartupJitterStaysWithinOneInterval(t *testing.T) {
	interval := 60 * time.Second
	seen := map[time.Duration]bool{}
	for range 200 {
		jitter := startupJitter(interval)
		if jitter < 0 || jitter >= interval {
			t.Fatalf("jitter %v outside [0, %v)", jitter, interval)
		}
		seen[jitter] = true
	}
	if len(seen) < 2 {
		t.Fatal("the jitter is not random: every host would still push on the same second")
	}
	if got := startupJitter(0); got != 0 {
		t.Fatalf("jitter with no interval = %v, expected 0", got)
	}
}

// CYAG-28: the push cadence waits the jitter, then pushes right away instead of waiting a further
// interval, so the gap after the immediate first push never exceeds one interval.
func TestPushCadenceStartsAfterTheJitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stop := make(chan struct{})
		defer close(stop)
		var pushes atomic.Int64
		jitter := 17 * time.Second

		go supervise(stop, time.Minute, jitter, func() { pushes.Add(1) }, func() {})

		time.Sleep(jitter - time.Nanosecond)
		synctest.Wait()
		if n := pushes.Load(); n != 0 {
			t.Fatalf("%d pushes before the %v jitter elapsed", n, jitter)
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if n := pushes.Load(); n != 1 {
			t.Fatalf("pushes when the jitter elapsed = %d, expected 1", n)
		}

		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()
		if n := pushes.Load(); n != 1 {
			t.Fatalf("pushes before the first interval after the jitter = %d, expected 1", n)
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if n := pushes.Load(); n != 2 {
			t.Fatalf("pushes one interval after the jitter = %d, expected 2", n)
		}
	})
}

// Closing stop during the jitter ends supervise without a single push.
func TestSuperviseStopsDuringTheJitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stop := make(chan struct{})
		done := make(chan struct{})
		var pushes atomic.Int64

		go func() {
			supervise(stop, time.Hour, time.Hour, func() { pushes.Add(1) }, func() {})
			close(done)
		}()
		close(stop)
		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("supervise did not return when stop was closed during the jitter")
		}
		if n := pushes.Load(); n != 0 {
			t.Fatalf("pushed %d times while stopping", n)
		}
	})
}

// CYAG-28: the pause after a 429 honours the server's wait, but a day (or a year) must not silence
// the host: it is capped at the same long idle used for a revoked host.
func TestRateLimitPauseHonoursRetryAfterWithinTheCap(t *testing.T) {
	cases := []struct {
		retryAfter, want time.Duration
	}{
		{0, 0}, // no wait given: the next tick retries
		{-time.Second, 0},
		{30 * time.Second, 30 * time.Second},
		{24 * time.Hour, revokedIdle},
		{time.Duration(1<<63 - 1), revokedIdle},
	}
	for _, tc := range cases {
		if got := rateLimitPause(tc.retryAfter); got != tc.want {
			t.Errorf("rateLimitPause(%v) = %v, expected %v", tc.retryAfter, got, tc.want)
		}
	}
}

// CYAG-28: the push loop really goes quiet after a 429 (or a 403) and comes back once the pause is
// over. The first push is refused, every later one succeeds; with a one-second interval, a missing
// pause would show up as a push every second.
func TestPushLoopHoldsAfterTheServerAsksToSlowDown(t *testing.T) {
	const interval = time.Second
	cases := []struct {
		name  string
		err   error
		quiet time.Duration
	}{
		{"429 with Retry-After 30s", &pusher.RateLimitedError{RetryAfter: 30 * time.Second}, 30 * time.Second},
		{"429 with Retry-After of a day is capped", &pusher.RateLimitedError{RetryAfter: 24 * time.Hour}, revokedIdle},
		{"429 without Retry-After retries on the next tick", &pusher.RateLimitedError{}, 0},
		{"403 revoked", pusher.ErrRevoked, revokedIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				stop := make(chan struct{})
				defer close(stop)
				var pushes atomic.Int64
				pushTick := func() {
					if pushes.Add(1) == 1 && !backOff(tc.err) {
						t.Errorf("backOff did not handle %v", tc.err)
					}
				}

				go supervise(stop, interval, 0, pushTick, func() {})

				if tc.quiet > 0 {
					time.Sleep(tc.quiet - time.Nanosecond)
					synctest.Wait()
					if n := pushes.Load(); n != 1 {
						t.Fatalf("%d pushes during the %v pause, expected only the refused one", n, tc.quiet)
					}
				}

				// the pause is over: the cadence starts again and the next tick pushes
				time.Sleep(time.Nanosecond + interval)
				synctest.Wait()
				if n := pushes.Load(); n != 2 {
					t.Fatalf("pushes one interval after the %v pause = %d, expected 2", tc.quiet, n)
				}
			})
		})
	}
}

// Any other push error is not a request to slow down: backOff leaves it to the caller, which logs it
// and lets the next tick try again.
func TestBackOffIgnoresOtherErrors(t *testing.T) {
	for _, err := range []error{errors.New("push failed: HTTP 500"), pusher.ErrHostCredentialRejected} {
		if backOff(err) {
			t.Errorf("backOff handled %v as a request to slow down", err)
		}
	}
}

// loopUntil must stop when stop is closed, without leaving hanging goroutines.
func TestLoopUntilStopsWhenStopIsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stop := make(chan struct{})
		done := make(chan struct{})
		var ticks atomic.Int64

		go func() {
			loopUntil(stop, time.Second, func() { ticks.Add(1) })
			close(done)
		}()

		time.Sleep(3*time.Second + time.Millisecond)
		synctest.Wait()
		if n := ticks.Load(); n != 3 {
			t.Fatalf("loopUntil ran fn %d times in 3 intervals, expected 3", n)
		}
		close(stop)
		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("loopUntil did not return after stop was closed")
		}
	})
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
