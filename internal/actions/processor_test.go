package actions

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	claims      int
}

func (f *fakeClient) Claim() (*Action, error) {
	f.claims++
	return f.claim, f.claimErr
}
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

// countingRunner counts the commands run. onRun, when set, runs inside the command: it lets a
// test look at the disk while the command is running, or simulate the agent being killed.
type countingRunner struct {
	runs  int
	onRun func()
}

func (r *countingRunner) Run(context.Context, string, ...string) ([]byte, error) {
	r.runs++
	if r.onRun != nil {
		r.onRun()
	}
	return []byte("ok"), nil
}

// agentKilled is the panic used to simulate the agent stopping in the middle of a command.
type agentKilled struct{}

// tickUntilKilled runs one Tick and swallows the simulated kill, as if the process died there.
func tickUntilKilled(t *testing.T, p Processor) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(agentKilled); !ok {
				panic(r)
			}
		}
	}()
	p.Tick()
}

func processorOn(dir string, client ActionClient, runner Runner) Processor {
	return Processor{
		endpoint:  "https://example.test",
		tokens:    fakeTokens{token: "cyi_h_1"},
		executor:  NewExecutorWithRunner(runner),
		pending:   NewPendingStore(dir),
		newClient: func(string, string) ActionClient { return client },
	}
}

// The start note is on disk before the command runs.
func TestStartNoteIsWrittenBeforeRunning(t *testing.T) {
	client := &fakeClient{claim: &Action{ID: "act_1", Kind: "apply_all_updates"}}
	dir := t.TempDir()
	var seen *Pending
	runner := &countingRunner{onRun: func() { seen, _ = NewPendingStore(dir).Load() }}

	processorOn(dir, client, runner).Tick()

	if seen == nil || !seen.Started || seen.ActionID != "act_1" || seen.Kind != "apply_all_updates" {
		t.Fatalf("no start note on disk while the command ran: %+v", seen)
	}
}

// Scenario 3: a command completed without interruption reports its result as before, and
// nothing is marked as interrupted.
func TestCompletedActionIsNotMarkedInterrupted(t *testing.T) {
	client := &fakeClient{claim: &Action{ID: "act_1", Kind: "apply_all_updates"}}
	dir := t.TempDir()
	runner := &countingRunner{}
	p := processorOn(dir, client, runner)

	p.Tick()
	client.claim = nil
	p.Tick()

	if runner.runs != 1 {
		t.Fatalf("command run %d times, expected 1", runner.runs)
	}
	if len(client.completed) != 1 || client.completed[0].result.Status != "succeeded" {
		t.Fatalf("expected one succeeded report: %+v", client.completed)
	}
	if strings.Contains(client.completed[0].result.Error, "interrupted") {
		t.Fatalf("completed action reported as interrupted: %+v", client.completed[0])
	}
	if pending, _ := NewPendingStore(dir).Load(); pending != nil {
		t.Fatalf("outbox not cleared: %+v", pending)
	}
}

// Scenario 1: the agent is restarted in the middle of an update. On the next start the update
// is not run again, and the panel receives it as failed with the interruption message.
func TestInterruptedUpdateIsReportedAndNeverRerun(t *testing.T) {
	action := &Action{ID: "act_upd", Kind: "apply_all_updates"}
	client := &fakeClient{claim: action}
	dir := t.TempDir()
	runner := &countingRunner{onRun: func() { panic(agentKilled{}) }}

	tickUntilKilled(t, processorOn(dir, client, runner))
	if len(client.completed) != 0 {
		t.Fatalf("nothing should have been reported before the kill: %+v", client.completed)
	}

	// Restart: a new Processor on the same data directory, two rounds. The claimed action is
	// no longer in the panel queue: only the note on disk could bring it back.
	runner.onRun = nil
	client.claim = nil
	restarted := processorOn(dir, client, runner)
	restarted.Tick()
	restarted.Tick()

	if runner.runs != 1 {
		t.Fatalf("command run %d times across the restart, expected exactly 1", runner.runs)
	}
	if len(client.completed) != 1 || client.completed[0].id != "act_upd" {
		t.Fatalf("interrupted action not reported once: %+v", client.completed)
	}
	got := client.completed[0].result
	if got.Status != "failed" || !strings.Contains(got.Error, "interrupted") || !strings.Contains(got.Error, "not run again") {
		t.Fatalf("unexpected interrupted result: %+v", got)
	}
	if pending, _ := NewPendingStore(dir).Load(); pending != nil {
		t.Fatalf("outbox not cleared after reporting the interruption: %+v", pending)
	}
}

// Scenario 2: a reboot stops the agent before it records the result. When the server is back
// the reboot is not repeated, and the message says the server may have rebooted anyway.
func TestInterruptedRebootIsNotRepeated(t *testing.T) {
	client := &fakeClient{claim: &Action{ID: "act_reboot", Kind: "reboot"}}
	dir := t.TempDir()
	runner := &countingRunner{onRun: func() { panic(agentKilled{}) }}

	tickUntilKilled(t, processorOn(dir, client, runner))

	runner.onRun = nil
	client.claim = nil
	processorOn(dir, client, runner).Tick()

	if runner.runs != 1 {
		t.Fatalf("reboot run %d times, expected exactly 1", runner.runs)
	}
	if len(client.completed) != 1 || client.completed[0].id != "act_reboot" {
		t.Fatalf("interrupted reboot not reported: %+v", client.completed)
	}
	got := client.completed[0].result
	if got.Status != "failed" || !strings.Contains(got.Error, "may have rebooted anyway") {
		t.Fatalf("reboot message should say the server may have rebooted anyway: %+v", got)
	}
}

// Scenario 4: when the start note cannot be written, the command does not run and nothing is
// reported (the server puts the action back in the queue); the error goes to the log.
func TestUnsavableStartNoteRunsNothing(t *testing.T) {
	client := &fakeClient{claim: &Action{ID: "act_1", Kind: "apply_all_updates"}}
	runner := &countingRunner{}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	processorOn(filepath.Join(t.TempDir(), "missing"), client, runner).Tick()

	if runner.runs != 0 {
		t.Fatalf("command run %d times without a start note", runner.runs)
	}
	if len(client.completed) != 0 {
		t.Fatalf("no report expected: %+v", client.completed)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "start note failed") {
		t.Fatalf("expected an error in the agent log, got: %s", logs.String())
	}
}

// Scenario 5: interrupted action and panel unreachable. The agent retries on every round,
// claims nothing new until the interruption is reported, and never runs the action again.
func TestInterruptionReportRetriedBeforeClaiming(t *testing.T) {
	dir := t.TempDir()
	if err := NewPendingStore(dir).Save(Pending{ActionID: "act_upd", Kind: "apply_all_updates", Started: true}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{
		claim:       &Action{ID: "act_new", Kind: "apply_security_updates"},
		completeErr: errors.New("network down"),
	}
	runner := &countingRunner{}
	p := processorOn(dir, client, runner)

	for range 3 {
		p.Tick()
	}
	if client.claims != 0 || runner.runs != 0 {
		t.Fatalf("claimed %d / ran %d while the interruption was unreported", client.claims, runner.runs)
	}
	if pending, _ := NewPendingStore(dir).Load(); pending == nil || !pending.Started {
		t.Fatalf("start note lost while the panel was unreachable: %+v", pending)
	}

	client.completeErr = nil
	p.Tick()

	if len(client.completed) != 5 || client.completed[3].id != "act_upd" || client.completed[3].result.Status != "failed" {
		t.Fatalf("interruption not reported once the panel came back: %+v", client.completed)
	}
	// Same round: only now the new action is claimed and run — once, and it is not act_upd.
	if client.claims != 1 || runner.runs != 1 || client.completed[4].id != "act_new" {
		t.Fatalf("claims=%d runs=%d completed=%+v", client.claims, runner.runs, client.completed)
	}
}

// A permanent rejection from the panel (e.g. the action expired) must not block the action
// loop forever: the note is dropped and the agent moves on, without running the action again.
func TestPermanentRejectionDoesNotBlockTheLoop(t *testing.T) {
	dir := t.TempDir()
	if err := NewPendingStore(dir).Save(Pending{ActionID: "act_gone", Started: true}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{completeErr: &HTTPError{Op: "complete", StatusCode: http.StatusNotFound}}
	runner := &countingRunner{}

	processorOn(dir, client, runner).Tick()

	if pending, _ := NewPendingStore(dir).Load(); pending != nil {
		t.Fatalf("permanently rejected note not dropped: %+v", pending)
	}
	if client.claims != 1 || runner.runs != 0 {
		t.Fatalf("claims=%d runs=%d: the loop should move on without rerunning", client.claims, runner.runs)
	}
}

// A transient failure (5xx, 401, 429) keeps the note for the next round.
func TestTransientRejectionKeepsTheNote(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusTooManyRequests} {
		dir := t.TempDir()
		if err := NewPendingStore(dir).Save(Pending{ActionID: "act_1", Started: true}); err != nil {
			t.Fatal(err)
		}
		client := &fakeClient{completeErr: &HTTPError{Op: "complete", StatusCode: code}}

		processorOn(dir, client, &countingRunner{}).Tick()

		if pending, _ := NewPendingStore(dir).Load(); pending == nil || client.claims != 0 {
			t.Fatalf("HTTP %d: note dropped or claim made (pending=%+v claims=%d)", code, pending, client.claims)
		}
	}
}

// A result file written by an older agent (no start note field) is resent as the finished
// result it is, not as an interruption.
func TestOldFormatResultIsResentAsIs(t *testing.T) {
	dir := t.TempDir()
	old := `{"action_id":"act_old","result":{"status":"succeeded","exit_code":0,"output":"done"}}`
	if err := os.WriteFile(filepath.Join(dir, "pending-action-result.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{}

	processorOn(dir, client, &countingRunner{}).Tick()

	want := Result{Status: "succeeded", ExitCode: 0, Output: "done"}
	if len(client.completed) != 1 || client.completed[0].id != "act_old" || client.completed[0].result != want {
		t.Fatalf("old-format result not resent as is: %+v", client.completed)
	}
}
