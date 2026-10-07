package actions

import (
	"errors"
	"testing"
)

type fakeTokens struct {
	token string
	err   error
}

func (f fakeTokens) Load() (string, error) { return f.token, f.err }

type completedCall struct {
	id     string
	result Result
}

type fakeClient struct {
	claim       *Action
	claimErr    error
	completeErr error
	completed   []completedCall
}

func (f *fakeClient) Claim() (*Action, error) { return f.claim, f.claimErr }
func (f *fakeClient) Complete(id string, result Result) error {
	f.completed = append(f.completed, completedCall{id: id, result: result})
	return f.completeErr
}

func newProcessor(t *testing.T, tokens TokenLoader, client ActionClient, runner Runner) (Processor, PendingStore) {
	t.Helper()
	store := NewPendingStore(t.TempDir())
	p := Processor{
		endpoint:  "https://example.test",
		tokens:    tokens,
		executor:  NewExecutorWithRunner(runner),
		pending:   store,
		newClient: func(string, string) ActionClient { return client },
	}
	return p, store
}

// DoD: the action result reaches the server and the outbox is cleared.
func TestActionResultIsReportedAndOutboxCleared(t *testing.T) {
	client := &fakeClient{claim: &Action{ID: "act_1", Kind: "apply_security_updates"}}
	p, store := newProcessor(t, fakeTokens{token: "cyi_h_1"}, client, &fakeRunner{out: []byte("ok")})

	p.Tick()

	if len(client.completed) != 1 || client.completed[0].id != "act_1" {
		t.Fatalf("result not reported to the server: %+v", client.completed)
	}
	if got := client.completed[0].result.Status; got != "succeeded" {
		t.Fatalf("result status = %q, expected succeeded", got)
	}
	if pending, _ := store.Load(); pending != nil {
		t.Fatalf("outbox not cleared after the report: %+v", pending)
	}
}

// Edge case: without a per-host credential the Processor must not even contact the server.
func TestNoCredentialMeansNoServerContact(t *testing.T) {
	p := Processor{
		endpoint: "https://example.test",
		tokens:   fakeTokens{token: ""},
		executor: NewExecutorWithRunner(&fakeRunner{}),
		pending:  NewPendingStore(t.TempDir()),
		newClient: func(string, string) ActionClient {
			t.Fatal("no client must be created without a credential")
			return nil
		},
	}
	p.Tick()
}

// Edge case: a result left pending by the previous round is resent before claiming.
func TestPendingResultIsResent(t *testing.T) {
	client := &fakeClient{} // no new queued action (Claim -> nil)
	p, store := newProcessor(t, fakeTokens{token: "cyi_h_1"}, client, &fakeRunner{})
	if err := store.Save(Pending{ActionID: "act_prev", Result: Result{Status: "succeeded"}}); err != nil {
		t.Fatal(err)
	}

	p.Tick()

	if len(client.completed) != 1 || client.completed[0].id != "act_prev" {
		t.Fatalf("pending result not resent: %+v", client.completed)
	}
	if pending, _ := store.Load(); pending != nil {
		t.Fatalf("outbox not cleared after resending: %+v", pending)
	}
}

// Edge case: no queued action (HTTP 204) -> no command run, no report.
func TestNoQueuedActionRunsNothing(t *testing.T) {
	client := &fakeClient{claim: nil}
	runner := &fakeRunner{}
	p, _ := newProcessor(t, fakeTokens{token: "cyi_h_1"}, client, runner)

	p.Tick()

	if runner.name != "" {
		t.Fatalf("no command should have run: %q", runner.name)
	}
	if len(client.completed) != 0 {
		t.Fatalf("no report expected: %+v", client.completed)
	}
}

// DoD: if the report fails, the result stays in the outbox and is resent — it arrives anyway.
func TestFailedReportLeavesResultInOutbox(t *testing.T) {
	client := &fakeClient{
		claim:       &Action{ID: "act_1", Kind: "apply_security_updates"},
		completeErr: errors.New("network down"),
	}
	p, store := newProcessor(t, fakeTokens{token: "cyi_h_1"}, client, &fakeRunner{out: []byte("ok")})

	p.Tick()

	pending, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || pending.ActionID != "act_1" {
		t.Fatalf("the result should have stayed in the outbox for the retry: %+v", pending)
	}
}

// Edge case: a failed claim must not run any action.
func TestFailedClaimRunsNoAction(t *testing.T) {
	client := &fakeClient{claimErr: errors.New("HTTP 500")}
	runner := &fakeRunner{}
	p, _ := newProcessor(t, fakeTokens{token: "cyi_h_1"}, client, runner)

	p.Tick()

	if runner.name != "" {
		t.Fatalf("no command should have run after a failed claim: %q", runner.name)
	}
}
