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
	// completeErrs, when set for an action id, overrides completeErr for that action only.
	completeErrs map[string]error
	completed    []completedCall
	claims       int
}

func (f *fakeClient) Claim() (*Action, error) {
	f.claims++
	return f.claim, f.claimErr
}
func (f *fakeClient) Complete(id string, result Result) error {
	f.completed = append(f.completed, completedCall{id: id, result: result})
	if err, ok := f.completeErrs[id]; ok {
		return err
	}
	return f.completeErr
}

// captureLogs redirects the default logger to a buffer for the duration of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
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
	logs := captureLogs(t)

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

// A transient failure (5xx, 401, 429, and any 4xx outside the three permanent answers) keeps
// the note for the next round.
func TestTransientRejectionKeepsTheNote(t *testing.T) {
	for _, code := range []int{
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusUnauthorized,
		http.StatusForbidden, http.StatusBadRequest, http.StatusTooManyRequests,
	} {
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

// CYAG-27 scenario 1: the host was removed and added again to the panel, so the pending result
// belongs to an action the panel no longer knows (404). The result is dropped without touching
// the server by hand, and the new action is claimed and run in the same round.
func TestPendingResultOfUnknownActionIsDroppedAndNewActionRuns(t *testing.T) {
	dir := t.TempDir()
	if err := NewPendingStore(dir).Save(Pending{ActionID: "act_old", Kind: "apply_all_updates", Result: Result{Status: "succeeded"}}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{
		claim:        &Action{ID: "act_new", Kind: "apply_all_updates"},
		completeErrs: map[string]error{"act_old": &HTTPError{Op: "complete", StatusCode: http.StatusNotFound}},
	}
	runner := &countingRunner{}
	logs := captureLogs(t)

	processorOn(dir, client, runner).Tick()

	if client.claims != 1 || runner.runs != 1 {
		t.Fatalf("claims=%d runs=%d: the new action should run in the same round", client.claims, runner.runs)
	}
	if len(client.completed) != 2 || client.completed[1].id != "act_new" || client.completed[1].result.Status != "succeeded" {
		t.Fatalf("new action not reported: %+v", client.completed)
	}
	if pending, _ := NewPendingStore(dir).Load(); pending != nil {
		t.Fatalf("outbox not empty after the round: %+v", pending)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "id=act_old") {
		t.Fatalf("the dropped result should leave an error in the agent log, got: %s", logs.String())
	}
}

// CYAG-27 scenario 2: the panel refuses the pending result as a transition not allowed (422)
// or as a gone action (410). The result is dropped with an error in the log, and the following
// actions start again normally.
func TestPermanentlyRejectedPendingResultIsDroppedAndLogged(t *testing.T) {
	for _, code := range []int{http.StatusUnprocessableEntity, http.StatusGone} {
		dir := t.TempDir()
		if err := NewPendingStore(dir).Save(Pending{ActionID: "act_old", Kind: "reboot", Result: Result{Status: "failed", ExitCode: 1}}); err != nil {
			t.Fatal(err)
		}
		client := &fakeClient{completeErrs: map[string]error{"act_old": &HTTPError{Op: "complete", StatusCode: code}}}
		runner := &countingRunner{}
		logs := captureLogs(t)
		p := processorOn(dir, client, runner)

		p.Tick()

		if pending, _ := NewPendingStore(dir).Load(); pending != nil {
			t.Fatalf("HTTP %d: rejected result not dropped: %+v", code, pending)
		}
		if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "dropping it") ||
			!strings.Contains(logs.String(), "id=act_old") {
			t.Fatalf("HTTP %d: expected an error naming the dropped result, got: %s", code, logs.String())
		}

		client.claim = &Action{ID: "act_next", Kind: "apply_security_updates"}
		p.Tick()

		if runner.runs != 1 || len(client.completed) != 2 || client.completed[1].id != "act_next" {
			t.Fatalf("HTTP %d: following action did not start again: runs=%d completed=%+v", code, runner.runs, client.completed)
		}
	}
}

// CYAG-27 scenario 3: with the panel unreachable, the finished result stays on disk, nothing
// new is claimed, and the result reaches the panel when the network is back.
func TestPendingResultKeptWhileNetworkIsDown(t *testing.T) {
	dir := t.TempDir()
	saved := Pending{ActionID: "act_1", Kind: "apply_all_updates", Result: Result{Status: "succeeded", Output: "done"}}
	if err := NewPendingStore(dir).Save(saved); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{
		claim:       &Action{ID: "act_new", Kind: "apply_security_updates"},
		completeErr: errors.New("dial tcp: connect: network is unreachable"),
	}
	runner := &countingRunner{}
	p := processorOn(dir, client, runner)

	for range 3 {
		p.Tick()
	}
	if client.claims != 0 || runner.runs != 0 {
		t.Fatalf("claimed %d / ran %d while the result was unreported", client.claims, runner.runs)
	}
	if pending, _ := NewPendingStore(dir).Load(); pending == nil || *pending != saved {
		t.Fatalf("result lost while the network was down: %+v", pending)
	}

	client.completeErr = nil
	client.claim = nil
	p.Tick()

	last := client.completed[len(client.completed)-1]
	if last.id != "act_1" || last.result != saved.Result {
		t.Fatalf("result did not reach the panel once the network came back: %+v", client.completed)
	}
	if pending, _ := NewPendingStore(dir).Load(); pending != nil {
		t.Fatalf("outbox not cleared after the delivery: %+v", pending)
	}
}

// CYAG-27 scenario 4: an internal panel error or an invalid credential is transient: the
// finished result stays on disk and is retried on the next round.
func TestPendingResultRetriedAfterServerOrCredentialError(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		dir := t.TempDir()
		saved := Pending{ActionID: "act_1", Kind: "apply_all_updates", Result: Result{Status: "succeeded"}}
		if err := NewPendingStore(dir).Save(saved); err != nil {
			t.Fatal(err)
		}
		client := &fakeClient{completeErr: &HTTPError{Op: "complete", StatusCode: code}}
		p := processorOn(dir, client, &countingRunner{})

		p.Tick()
		if pending, _ := NewPendingStore(dir).Load(); pending == nil || *pending != saved || client.claims != 0 {
			t.Fatalf("HTTP %d: result dropped or claim made (pending=%+v claims=%d)", code, pending, client.claims)
		}

		client.completeErr = nil
		p.Tick()
		if len(client.completed) != 2 || client.completed[1].id != "act_1" {
			t.Fatalf("HTTP %d: result not retried on the next round: %+v", code, client.completed)
		}
		if pending, _ := NewPendingStore(dir).Load(); pending != nil {
			t.Fatalf("HTTP %d: outbox not cleared after the retry: %+v", code, pending)
		}
	}
}

// CYAG-27 scenario 5: the panel refuses for good the result of an action just run. The result
// does not stay pending, and the next round claims the new action straight away.
func TestJustProducedResultRejectedForGoodDoesNotStayPending(t *testing.T) {
	dir := t.TempDir()
	client := &fakeClient{
		claim:        &Action{ID: "act_1", Kind: "apply_all_updates"},
		completeErrs: map[string]error{"act_1": &HTTPError{Op: "complete", StatusCode: http.StatusUnprocessableEntity}},
	}
	runner := &countingRunner{}
	logs := captureLogs(t)
	p := processorOn(dir, client, runner)

	p.Tick()

	if pending, _ := NewPendingStore(dir).Load(); pending != nil {
		t.Fatalf("result rejected for good stayed pending: %+v", pending)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "dropping it") {
		t.Fatalf("expected an error for the dropped result, got: %s", logs.String())
	}

	client.claim = &Action{ID: "act_2", Kind: "apply_security_updates"}
	p.Tick()

	if client.claims != 2 || runner.runs != 2 {
		t.Fatalf("claims=%d runs=%d: the next round should claim the new action", client.claims, runner.runs)
	}
	if len(client.completed) != 2 || client.completed[1].id != "act_2" {
		t.Fatalf("the rejected result was resent or the new one not reported: %+v", client.completed)
	}
}

// A transient failure on the result just produced keeps it pending, as before.
func TestJustProducedResultKeptOnTransientRejection(t *testing.T) {
	dir := t.TempDir()
	client := &fakeClient{
		claim:       &Action{ID: "act_1", Kind: "apply_all_updates"},
		completeErr: &HTTPError{Op: "complete", StatusCode: http.StatusServiceUnavailable},
	}

	processorOn(dir, client, &countingRunner{}).Tick()

	if pending, _ := NewPendingStore(dir).Load(); pending == nil || pending.ActionID != "act_1" {
		t.Fatalf("result dropped on a transient failure: %+v", pending)
	}
}
