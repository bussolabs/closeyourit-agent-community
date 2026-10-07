package agent

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeFailedUnits injects a set of failed units without depending on the real systemd (cross-platform tests).
type fakeFailedUnits struct{ units []failedUnit }

func (f *fakeFailedUnits) failedUnits() []failedUnit { return f.units }

// writeFakeJournalctl creates a script that prints `output` ignoring the arguments: it replaces the
// real journalctl binary in tests (collectStream/captureUnit run it via exec).
func writeFakeJournalctl(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(outFile, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// writeSystemd255Journalctl replicates the systemd 255 journalctl: it INSPECTS the arguments and, if
// it receives --since and --after-cursor together, exits 1 with the real error on stderr (as in
// production); otherwise it prints `output`. Unlike writeFakeJournalctl it reacts to the real
// arguments, so the test exercises the real command construction instead of ignoring it.
func writeSystemd255Journalctl(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(outFile, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"since=0; after=0\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in\n" +
		"    --since=*) since=1 ;;\n" +
		"    --after-cursor=*) after=1 ;;\n" +
		"  esac\n" +
		"done\n" +
		"if [ \"$since\" = 1 ] && [ \"$after\" = 1 ]; then\n" +
		"  echo 'Please specify only one of --since=, --cursor=, and --after-cursor=.' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"cat \"" + outFile + "\"\n"
	path := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeFailingJournalctl creates a script that exits 1 printing a message on stderr with no output:
// it simulates a journalctl rejecting the command (failed collection).
func writeFailingJournalctl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'journalctl: command rejected' >&2\nexit 1\n"
	path := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeCursorRotatedJournalctl simulates a cursor rotated away: if it receives --after-cursor it
// exits 1 with the seek error on stderr (the cursor is no longer in the journal), while with --since
// (no cursor) it prints `output`. It exercises the fallback that unblocks the stream.
func writeCursorRotatedJournalctl(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(outFile, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in\n" +
		"    --after-cursor=*)\n" +
		"      echo 'Failed to seek to cursor: Invalid argument' >&2\n" +
		"      exit 1 ;;\n" +
		"  esac\n" +
		"done\n" +
		"cat \"" + outFile + "\"\n"
	path := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureSlog redirects the default logger to a buffer (Warn+ level) for the duration of the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func newTestJournalManager(journalctlPath, dataDir string, sd failedUnitsSource) *journalManager {
	return &journalManager{
		journalctlPath: journalctlPath,
		priority:       3,
		tail:           50,
		dataDir:        dataDir,
		systemd:        sd,
		sentSnapshots:  make(map[string]bool),
	}
}

func TestParseJournalLine(t *testing.T) {
	t.Run("valid line: string fields (PRIORITY/timestamp), unit and message", func(t *testing.T) {
		line := []byte(`{"__CURSOR":"c1","__REALTIME_TIMESTAMP":"1782986400000000","PRIORITY":"3","_SYSTEMD_UNIT":"backup.service","MESSAGE":"boom"}`)
		cursor, entry, keep := parseJournalLine(line, 3)
		if !keep || cursor != "c1" {
			t.Fatalf("expected keep+cursor c1, got keep=%v cursor=%q", keep, cursor)
		}
		if entry.Time != 1782986400000000 || entry.Priority != 3 || entry.Unit != "backup.service" || entry.Message != "boom" {
			t.Fatalf("unexpected entry: %+v", entry)
		}
	})

	t.Run("MESSAGE as a byte array (non-UTF8 journald) is decoded", func(t *testing.T) {
		line := []byte(`{"__CURSOR":"c2","PRIORITY":"3","MESSAGE":[104,105]}`)
		_, entry, keep := parseJournalLine(line, 3)
		if !keep || entry.Message != "hi" {
			t.Fatalf("expected message 'hi', got keep=%v msg=%q", keep, entry.Message)
		}
	})

	t.Run("null byte in the message is removed (Postgres rejects it)", func(t *testing.T) {
		line := []byte(`{"__CURSOR":"c3","PRIORITY":"3","MESSAGE":"a\u0000b"}`)
		_, entry, _ := parseJournalLine(line, 3)
		if entry.Message != "ab" {
			t.Fatalf("expected 'ab', got %q", entry.Message)
		}
	})

	t.Run("without __CURSOR it is malformed: empty cursor, discarded", func(t *testing.T) {
		cursor, _, keep := parseJournalLine([]byte(`{"PRIORITY":"3","MESSAGE":"x"}`), 3)
		if cursor != "" || keep {
			t.Fatalf("expected discard, got cursor=%q keep=%v", cursor, keep)
		}
	})

	t.Run("empty message: valid cursor but keep=false (the stream advances)", func(t *testing.T) {
		cursor, _, keep := parseJournalLine([]byte(`{"__CURSOR":"c4","PRIORITY":"3","MESSAGE":""}`), 3)
		if cursor != "c4" || keep {
			t.Fatalf("expected cursor c4 + keep=false, got cursor=%q keep=%v", cursor, keep)
		}
	})

	t.Run("priority above the maximum is discarded (client-side defense)", func(t *testing.T) {
		cursor, _, keep := parseJournalLine([]byte(`{"__CURSOR":"c5","PRIORITY":"5","MESSAGE":"warn"}`), 3)
		if cursor != "c5" || keep {
			t.Fatalf("expected discard of priority 5 > 3, got cursor=%q keep=%v", cursor, keep)
		}
	})

	t.Run("unit: _SYSTEMD_UNIT then SYSLOG_IDENTIFIER then kernel", func(t *testing.T) {
		_, e1, _ := parseJournalLine([]byte(`{"__CURSOR":"a","PRIORITY":"3","SYSLOG_IDENTIFIER":"sshd","MESSAGE":"x"}`), 3)
		if e1.Unit != "sshd" {
			t.Fatalf("expected fallback to SYSLOG_IDENTIFIER 'sshd', got %q", e1.Unit)
		}
		_, e2, _ := parseJournalLine([]byte(`{"__CURSOR":"b","PRIORITY":"3","MESSAGE":"x"}`), 3)
		if e2.Unit != "kernel" {
			t.Fatalf("expected fallback 'kernel', got %q", e2.Unit)
		}
	})
}

func TestScrubUTF8(t *testing.T) {
	if got := scrubUTF8("a\x00b"); got != "ab" {
		t.Fatalf("null byte not removed: %q", got)
	}
	bad := "ok" + string([]byte{0xff})
	got := scrubUTF8(bad)
	if !utf8.ValidString(got) || !strings.HasPrefix(got, "ok") {
		t.Fatalf("non-UTF8 scrub failed: %q valid=%v", got, utf8.ValidString(got))
	}
}

func TestClampRunes(t *testing.T) {
	if got := clampRunes("abcdef", 3); got != "abc" {
		t.Fatalf("clamp ascii: %q", got)
	}
	// "é" = 2 bytes, 1 rune: clamping by rune must not split the multibyte character.
	got := clampRunes("ééé", 2)
	if utf8.RuneCountInString(got) != 2 || !utf8.ValidString(got) {
		t.Fatalf("wrong multibyte clamp: %q (runes=%d)", got, utf8.RuneCountInString(got))
	}
}

func TestJournalEnvInt(t *testing.T) {
	if got := journalEnvInt("JOURNAL_PRIORITY", 3, 0, 7); got != 3 {
		t.Fatalf("expected default 3, got %d", got)
	}
	t.Setenv("JOURNAL_PRIORITY", "1")
	if got := journalEnvInt("JOURNAL_PRIORITY", 3, 0, 7); got != 1 {
		t.Fatalf("expected value 1, got %d", got)
	}
	t.Setenv("JOURNAL_PRIORITY", "99")
	if got := journalEnvInt("JOURNAL_PRIORITY", 3, 0, 7); got != 3 {
		t.Fatalf("out of range → default 3, got %d", got)
	}
}

func TestCollectStreamHeadCap(t *testing.T) {
	script := writeFakeJournalctl(t, genStreamLines(600))
	jm := newTestJournalManager(script, "", nil)

	entries, lastCursor := jm.collectStream()
	if len(entries) != journalMaxEntries {
		t.Fatalf("expected cap at %d entries, got %d", journalMaxEntries, len(entries))
	}
	// HEAD-mode: stops at the 500th line read (c0..c499), cursor = the 500th one.
	if lastCursor != "c499" {
		t.Fatalf("expected cursor c499 (500th line), got %q", lastCursor)
	}
}

func TestCollectStreamSkipsBlankAdvancesCursor(t *testing.T) {
	output := strings.Join([]string{
		`{"__CURSOR":"c1","PRIORITY":"3","_SYSTEMD_UNIT":"a.service","MESSAGE":"first"}`,
		`{"__CURSOR":"c2","PRIORITY":"3","MESSAGE":""}`, // blank → discarded but the cursor advances
		`{"__CURSOR":"c3","PRIORITY":"3","_SYSTEMD_UNIT":"a.service","MESSAGE":"third"}`,
	}, "\n")
	jm := newTestJournalManager(writeFakeJournalctl(t, output), "", nil)

	entries, lastCursor := jm.collectStream()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (blank discarded), got %d", len(entries))
	}
	if lastCursor != "c3" {
		t.Fatalf("cursor must advance past the blank up to c3, got %q", lastCursor)
	}
}

func TestStreamArgsNoSinceAfterCursorCombo(t *testing.T) {
	t.Run("first run without cursor: --since bounds, no --after-cursor", func(t *testing.T) {
		jm := newTestJournalManager("journalctl", "", nil)
		args := jm.streamArgs()
		if !slices.Contains(args, "--since=-48h") {
			t.Fatalf("first run must bound with --since=-48h, got %v", args)
		}
		if hasArgPrefix(args, "--after-cursor=") {
			t.Fatalf("first run must not have --after-cursor, got %v", args)
		}
	})

	t.Run("with cursor: --after-cursor alone, NEVER together with --since (systemd 255 rejects them)", func(t *testing.T) {
		jm := newTestJournalManager("journalctl", "", nil)
		jm.cursor = "c123"
		args := jm.streamArgs()
		if !slices.Contains(args, "--after-cursor=c123") {
			t.Fatalf("with a cursor it must resume from --after-cursor=c123, got %v", args)
		}
		if hasArgPrefix(args, "--since=") {
			t.Fatalf("with a cursor there must be NO --since (combination rejected by systemd 255), got %v", args)
		}
	})
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func TestCollectStreamResumesFromCursor(t *testing.T) {
	// Scenario 1: from the second send on a cursor exists. systemd 255 rejects --since + --after-cursor
	// together: the lines must arrive anyway (the bug had been making them disappear for weeks).
	output := strings.Join([]string{
		`{"__CURSOR":"c1","PRIORITY":"3","_SYSTEMD_UNIT":"a.service","MESSAGE":"boom"}`,
		`{"__CURSOR":"c2","PRIORITY":"3","_SYSTEMD_UNIT":"a.service","MESSAGE":"bang"}`,
	}, "\n")
	jm := newTestJournalManager(writeSystemd255Journalctl(t, output), "", nil)
	jm.cursor = "c0"

	entries, lastCursor := jm.collectStream()
	if len(entries) != 2 {
		t.Fatalf("with an active cursor the lines must arrive (2 expected), got %d", len(entries))
	}
	if lastCursor != "c2" {
		t.Fatalf("the cursor must advance to c2, got %q", lastCursor)
	}
}

func TestCollectStreamRotatedCursorFallsBackToSince(t *testing.T) {
	// If the cursor points to rotated-away data, journalctl --after-cursor fails: the stream must NOT
	// stay stuck forever. It must resume once with --since=-48h and the lines must arrive
	// (self-recovery), with the server deduplicating on [host, cursor].
	logs := captureSlog(t)
	output := `{"__CURSOR":"n1","PRIORITY":"3","_SYSTEMD_UNIT":"a.service","MESSAGE":"recovered"}`
	jm := newTestJournalManager(writeCursorRotatedJournalctl(t, output), "", nil)
	jm.cursor = "old-rotated-away"

	entries, lastCursor := jm.collectStream()
	if len(entries) != 1 {
		t.Fatalf("after the fallback to --since the lines must arrive (1 expected), got %d", len(entries))
	}
	if lastCursor != "n1" {
		t.Fatalf("the cursor must advance to the new value n1, got %q", lastCursor)
	}
	if !strings.Contains(logs.String(), "Failed to seek to cursor") {
		t.Fatalf("the cursor failure must stay visible in the logs, log=%q", logs.String())
	}
}

func TestCollectStreamFailedCollectionIsVisible(t *testing.T) {
	// Scenario 2: if the collection command is rejected, the failure must NOT be silent:
	// an explicit error with the journalctl stderr must remain in the agent logs.
	logs := captureSlog(t)
	jm := newTestJournalManager(writeFailingJournalctl(t), "", nil)

	entries, lastCursor := jm.collectStream()
	if len(entries) != 0 {
		t.Fatalf("failed command: no entry expected, got %d", len(entries))
	}
	if lastCursor != jm.cursor {
		t.Fatalf("failed command: the cursor must not advance, got %q", lastCursor)
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("the failure must be logged at ERROR level, log=%q", out)
	}
	if !strings.Contains(out, "command rejected") {
		t.Fatalf("the log must carry the journalctl stderr, log=%q", out)
	}
}

func TestCollectAndCommitCursor(t *testing.T) {
	dataDir := t.TempDir()
	output := `{"__CURSOR":"cX","PRIORITY":"3","_SYSTEMD_UNIT":"a.service","MESSAGE":"boom"}`
	jm := newTestJournalManager(writeFakeJournalctl(t, output), dataDir, nil)

	report := jm.Collect()
	if report == nil || len(report.Entries) != 1 {
		t.Fatalf("Collect must produce 1 entry, got %+v", report)
	}
	// Before Commit the cursor does not exist on disk (push not confirmed yet).
	if _, err := os.Stat(filepath.Join(dataDir, journalCursorFile)); !os.IsNotExist(err) {
		t.Fatalf("cursor must not be persisted before Commit")
	}
	jm.Commit()
	persisted := newTestJournalManager("x", dataDir, nil).readCursor()
	if persisted != "cX" {
		t.Fatalf("after Commit the persisted cursor must be cX, got %q", persisted)
	}
}

func TestCommitNoOpOnFailedPush(t *testing.T) {
	dataDir := t.TempDir()
	output := `{"__CURSOR":"cY","PRIORITY":"3","_SYSTEMD_UNIT":"a.service","MESSAGE":"boom"}`
	jm := newTestJournalManager(writeFakeJournalctl(t, output), dataDir, nil)

	jm.Collect() // the push would fail: we do NOT call Commit
	if _, err := os.Stat(filepath.Join(dataDir, journalCursorFile)); !os.IsNotExist(err) {
		t.Fatalf("without Commit the cursor must not advance (resend)")
	}
}

func TestCollectSnapshotsLifecycle(t *testing.T) {
	snapshotOutput := "2026-07-02T10:00:00+0000 host systemd[1]: backup.service failed\n2026-07-02T10:00:01+0000 host systemd[1]: entered failed state"
	script := writeFakeJournalctl(t, snapshotOutput)
	sd := &fakeFailedUnits{units: []failedUnit{{UnitName: "backup.service", WireName: "backup"}}}
	jm := newTestJournalManager(script, "", sd)

	first := jm.collectSnapshots()
	if first == nil || len(first["backup"].Lines) != 2 {
		t.Fatalf("first failure: expected a snapshot with 2 lines, got %+v", first)
	}

	if again := jm.collectSnapshots(); again != nil {
		t.Fatalf("same unit still failed: no re-capture, got %+v", again)
	}

	sd.units = nil // recovered
	if recovered := jm.collectSnapshots(); recovered != nil {
		t.Fatalf("no failed unit → nil, got %+v", recovered)
	}
	if jm.sentSnapshots["backup"] {
		t.Fatalf("a recovered unit must leave the sent set (to re-capture on a new failure)")
	}

	sd.units = []failedUnit{{UnitName: "backup.service", WireName: "backup"}} // fails again
	if refail := jm.collectSnapshots(); refail == nil || len(refail["backup"].Lines) != 2 {
		t.Fatalf("failure again after recovery: expected a re-captured snapshot, got %+v", refail)
	}
}

func TestNewJournalManagerSkip(t *testing.T) {
	t.Setenv("SKIP_JOURNAL", "true")
	if jm := newJournalManager("", nil); jm != nil {
		t.Fatalf("SKIP_JOURNAL=true must disable the journal (nil)")
	}
}

func genStreamLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"__CURSOR":"c%d","__REALTIME_TIMESTAMP":"1782986400000000","PRIORITY":"3","_SYSTEMD_UNIT":"app.service","MESSAGE":"line %d"}`+"\n", i, i)
	}
	return b.String()
}
