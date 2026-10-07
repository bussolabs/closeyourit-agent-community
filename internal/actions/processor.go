package actions

import "log/slog"

// TokenLoader provides the per-host credential persisted on disk. The Processor reloads it
// on every Tick: this way the action goroutine shares no mutable state with the push loop,
// which remains the only one writing the credential after self-registration.
type TokenLoader interface {
	Load() (string, error)
}

// ActionClient is the subset of the HTTP Client used by the Processor. The interface
// makes the action loop testable with a double, without touching the network.
type ActionClient interface {
	Claim() (*Action, error)
	Complete(id string, result Result) error
}

// Processor runs one work round on server actions: it resends a pending result, then claims
// and runs at most one action. It runs in a dedicated goroutine with its own ticker, separate
// from the metrics push, so a long action (e.g. unattended-upgrade, up to 30 minutes) does not
// stop monitoring the machine.
type Processor struct {
	endpoint  string
	tokens    TokenLoader
	executor  Executor
	pending   PendingStore
	newClient func(endpoint, token string) ActionClient
}

// NewProcessor builds a Processor that talks to the server through the real HTTP Client.
func NewProcessor(endpoint string, tokens TokenLoader, executor Executor, pending PendingStore) Processor {
	return Processor{
		endpoint:  endpoint,
		tokens:    tokens,
		executor:  executor,
		pending:   pending,
		newClient: func(endpoint, token string) ActionClient { return NewClient(endpoint, token) },
	}
}

// Tick runs a single round of the action loop. It is safe to call it idle: without a per-host
// credential or without queued actions it does not even contact the server.
func (p Processor) Tick() {
	token, err := p.tokens.Load()
	if err != nil {
		slog.Error("Host credential unreadable", "err", err)
		return
	}
	if token == "" {
		return // host not registered yet: no action to claim
	}
	client := p.newClient(p.endpoint, token)

	if !p.flushPending(client) {
		return
	}

	claimed, err := client.Claim()
	if err != nil {
		slog.Warn("Action claim failed", "err", err)
		return
	}
	if claimed == nil {
		return // no queued action
	}

	slog.Info("Running action", "id", claimed.ID, "kind", claimed.Kind)
	result := p.executor.Execute(*claimed)
	// The result is persisted before reporting it: if the report fails (network down,
	// restart) the next Tick resends it from flushPending — the result arrives anyway.
	if err := p.pending.Save(Pending{ActionID: claimed.ID, Result: result}); err != nil {
		slog.Error("Persisting the action result failed", "id", claimed.ID, "err", err)
		return
	}
	if err := client.Complete(claimed.ID, result); err != nil {
		slog.Error("Action report failed", "id", claimed.ID, "err", err)
		return
	}
	if err := p.pending.Clear(); err != nil {
		slog.Error("Clearing the action outbox failed", "id", claimed.ID, "err", err)
	}
}

// flushPending resends the result left pending by the previous round. It returns false
// if something goes wrong: in that case the Tick stops and retries on the next round.
func (p Processor) flushPending(client ActionClient) bool {
	pending, err := p.pending.Load()
	if err != nil {
		slog.Error("Action outbox unreadable", "err", err)
		return false
	}
	if pending == nil {
		return true
	}
	if err := client.Complete(pending.ActionID, pending.Result); err != nil {
		slog.Warn("Resending the action result failed", "id", pending.ActionID, "err", err)
		return false
	}
	if err := p.pending.Clear(); err != nil {
		slog.Error("Clearing the action outbox failed", "err", err)
		return false
	}
	return true
}
