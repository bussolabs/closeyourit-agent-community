// closeyourit-agent: collects system metrics (collection derived from Beszel, MIT) and pushes them
// to CloseYourIt every CYI_INTERVAL. Subcommands: (default) run · health · version.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	serveractions "github.com/bussolabs/closeyourit-agent/internal/actions"
	"github.com/bussolabs/closeyourit-agent/internal/agent"
	"github.com/bussolabs/closeyourit-agent/internal/agent/health"
	"github.com/bussolabs/closeyourit-agent/internal/config"
	"github.com/bussolabs/closeyourit-agent/internal/credentials"
	"github.com/bussolabs/closeyourit-agent/internal/pusher"
	"github.com/bussolabs/closeyourit-agent/internal/version"
)

// After a 403 (host revoked) the agent does not hammer: it retries every 15 minutes (revocation is reversible).
const revokedIdle = 15 * time.Minute

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-v", "--version":
			fmt.Println(version.AppName, version.Version)
			return
		case "health":
			// exit 0 if the last successful push is recent (Docker HEALTHCHECK/systemd watchdog).
			dataDir, err := agent.GetDataDir()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if err := health.Check(dataDir); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("Invalid configuration", "err", err)
		os.Exit(1)
	}

	a, err := agent.NewAgent()
	if err != nil {
		slog.Error("Agent initialization failed", "err", err)
		os.Exit(1)
	}

	push := pusher.New(cfg.Endpoint, cfg.Token)
	dataDir, err := agent.GetDataDir()
	if err != nil {
		slog.Error("Data directory unavailable", "err", err)
		os.Exit(1)
	}
	credentialStore := credentials.New(dataDir)
	if hostToken, err := credentialStore.Load(); err != nil {
		slog.Error("Host credential unreadable", "err", err)
		os.Exit(1)
	} else if hostToken != "" {
		push.SetToken(hostToken)
	}
	executor := serveractions.NewExecutor()
	pendingStore := serveractions.NewPendingStore(dataDir)
	fingerprint := a.Fingerprint()
	cacheTimeMs := uint16(cfg.Interval.Milliseconds())

	slog.Info("closeyourit-agent started", "version", version.Version,
		"endpoint", cfg.Endpoint, "interval", cfg.Interval, "fingerprint", fingerprint)

	run := func() {
		host := pusher.HostInfoFromDetails(a.Details(), cfg.HostName)
		host.RebootRequired, host.UpdatesAvailable, host.SecurityUpdatesAvailable = agent.SystemUpdates()
		payload := &pusher.Payload{
			Fingerprint:  fingerprint,
			AgentVersion: version.Version,
			RecordedAt:   time.Now().UTC(),
			Host:         host,
			Data:         a.Collect(cacheTimeMs),
			Smart:        a.SmartData(),
			Journal:      a.JournalReport(),
			Database:     agent.DatabaseReport(),
		}
		if err := push.Push(payload); err != nil {
			if errors.Is(err, pusher.ErrRevoked) {
				slog.Warn("Host revoked by the server: long idle", "retry_in", revokedIdle)
				time.Sleep(revokedIdle)
				return
			}
			if errors.Is(err, pusher.ErrHostCredentialRejected) {
				if err := reenroll(credentialStore, push, cfg.Token); err != nil {
					slog.Error("Re-enrollment failed: per-host credential cannot be deleted", "err", err)
					return
				}
				slog.Warn("Per-host credential rejected (R401-SERVER-002): back to the enrollment token, re-enrolling on the next tick")
				return
			}
			slog.Warn("Push failed", "err", err)
			return
		}
		if issued := push.TakeHostToken(); issued != "" {
			if err := credentialStore.Save(issued); err != nil {
				slog.Error("Saving the host credential failed", "err", err)
				return
			}
			push.SetToken(issued)
			slog.Info("Per-host credential acquired")
		}
		// Only after a successful push does the journald cursor advance on disk: an undelivered stream
		// is sent again (the server deduplicates on [host, cursor]).
		a.CommitJournal()
		_ = health.Update(dataDir)
		slog.Debug("Push succeeded")
	}

	// The action loop runs in a dedicated goroutine with its own ticker (see supervise):
	// a long action (unattended-upgrade, up to 30 minutes) no longer stops the push, so the
	// machine keeps reporting and the false host-down alarm does not fire. The Processor
	// reloads the credential from credentialStore on every round: no state shared with the push.
	processor := serveractions.NewProcessor(cfg.Endpoint, credentialStore, executor, pendingStore)

	run() // first immediate push (self-registration)
	supervise(make(chan struct{}), cfg.Interval, run, processor.Tick)
}

// credentialResetter and bearerSetter are the only two capabilities re-enrollment uses: this way
// the disposition is tested without setting up either an on-disk store or an HTTP server.
type credentialResetter interface{ Clear() error }
type bearerSetter interface{ SetToken(string) }

// reenroll brings the agent back to the enrollment token after the server rejected the per-machine
// credential (401 R401-SERVER-002): it deletes the file and restores the fleet token, so the next
// tick self-registers by fingerprint as on first start. Non-negotiable order: deletion first. If the
// file stays, a restart would reload the dead credential from disk and the host would silently go
// mute again (CYAG-19).
func reenroll(store credentialResetter, push bearerSetter, enrollmentToken string) error {
	if err := store.Clear(); err != nil {
		return err
	}
	push.SetToken(enrollmentToken)
	return nil
}

// loopUntil runs fn on every interval tick, until stop is closed.
func loopUntil(stop <-chan struct{}, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			fn()
		}
	}
}

// supervise keeps the metrics push in the calling goroutine and starts the action loop in a
// dedicated goroutine. The two tickers are independent: a long action does not delay the push.
// The action loop runs a first round right away (the caller already did the immediate push with
// run()), so the outbox and any queued actions do not wait a whole interval at startup.
func supervise(stop <-chan struct{}, interval time.Duration, pushTick, actionTick func()) {
	go func() {
		actionTick()
		loopUntil(stop, interval, actionTick)
	}()
	loopUntil(stop, interval, pushTick)
}
