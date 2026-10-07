package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bussolabs/closeyourit-agent/internal/agent/utils"
	"github.com/bussolabs/closeyourit-agent/internal/entities/journal"
)

const (
	journalCursorFile  = "journal-cursor"
	journalDefaultPrio = 3 // err/crit/alert/emerg (syslog <= 3)
	journalDefaultTail = 50
	journalMaxEntries  = 500
	journalMaxBytes    = 256 * 1024
	journalMessageMax  = 1024 // runes, not bytes
	journalExecTimeout = 15 * time.Second
	journalScanBufMax  = 2 * 1024 * 1024 // a journald -o json line can be large
)

// failedUnit pairs the FULL unit name (key of the systemd map, argument of `journalctl -u`) with the
// WIRE name (== systemd.Service.Name, payload field `n` → snapshot key on the server side).
// The type lives here (cross-platform file) because the journalManager consumes it and both the
// linux systemdManager and the non-linux stub implement it.
type failedUnit struct {
	UnitName string
	WireName string
}

// failedUnitsSource is what the journalManager consumes from the systemd manager: the current failed units.
// An interface (not *systemdManager directly) to inject a fake in cross-platform tests.
type failedUnitsSource interface {
	failedUnits() []failedUnit
}

// journalManager collects native journald logs by shelling out to `journalctl` (CGO_ENABLED=0 →
// no sdjournal). Two responsibilities: (1) a priority <= err/crit stream with a forward-only cursor
// persisted on disk, (2) `journalctl -u <unit>` snapshots for systemd units that just failed.
// Not thread-safe: used only by the push loop (single goroutine); the underlying systemdManager has
// its own lock.
type journalManager struct {
	journalctlPath string
	priority       int
	tail           int
	dataDir        string
	systemd        failedUnitsSource
	sentSnapshots  map[string]bool // wire name → snapshot already sent (do not re-capture every minute)
	cursor         string          // last committed cursor (persisted)
	pending        string          // candidate cursor of the current Collect (committed on push OK)
}

// newJournalManager returns nil (feature disabled, no-op) if journalctl is not in PATH or if
// SKIP_JOURNAL=true. sd may be nil (no systemd): in that case no snapshots, only the stream.
func newJournalManager(dataDir string, sd *systemdManager) *journalManager {
	if skip, _ := utils.GetEnv("SKIP_JOURNAL"); skip == "true" {
		return nil
	}
	path, err := exec.LookPath("journalctl")
	if err != nil {
		slog.Debug("journalctl not found: journal disabled")
		return nil
	}
	jm := &journalManager{
		journalctlPath: path,
		priority:       journalEnvInt("JOURNAL_PRIORITY", journalDefaultPrio, 0, 7),
		tail:           journalEnvInt("JOURNAL_TAIL", journalDefaultTail, 1, journalMaxEntries),
		dataDir:        dataDir,
		sentSnapshots:  make(map[string]bool),
	}
	// Typed-nil guard: a nil *systemdManager assigned to an interface would be != nil (then panic
	// on failedUnits). Assign only if the manager really exists.
	if sd != nil {
		jm.systemd = sd
	}
	jm.cursor = jm.readCursor()
	return jm
}

// Collect reads the new stream and captures the snapshots of the units that just failed, without
// persisting the cursor (Commit does that after a successful push). It returns nil if there is
// nothing to send.
func (jm *journalManager) Collect() *journal.Report {
	entries, nextCursor := jm.collectStream()
	snapshots := jm.collectSnapshots()
	jm.pending = nextCursor

	if len(entries) == 0 && len(snapshots) == 0 {
		return nil
	}
	report := &journal.Report{}
	if len(entries) > 0 {
		report.Entries = entries
	}
	if len(snapshots) > 0 {
		report.Snapshots = snapshots
	}
	return report
}

// Commit persists the candidate cursor after a successful push. It must NOT be called on a failed
// push: the stream is resent on the next Collect (--after-cursor unchanged) and the server deduplicates.
func (jm *journalManager) Commit() {
	if jm.pending == "" || jm.pending == jm.cursor {
		return
	}
	if err := jm.saveCursor(jm.pending); err != nil {
		slog.Warn("Saving the journal cursor failed", "err", err)
		return
	}
	jm.cursor = jm.pending
}

// streamArgs builds the `journalctl` arguments for the stream. With an active cursor it resumes from
// --after-cursor ALONE: systemd 255 rejects --since and --after-cursor together ("Please specify only
// one of ...") with exit 1 and empty output, which is what wiped out collection from the second send
// onwards. Only on the first run (empty cursor: new agent) does --since=-48h bound the output.
func (jm *journalManager) streamArgs() []string {
	if jm.cursor != "" {
		return append(jm.baseStreamArgs(), "--after-cursor="+jm.cursor)
	}
	return jm.sinceArgs()
}

func (jm *journalManager) baseStreamArgs() []string {
	return []string{"-q", "-p", strconv.Itoa(jm.priority), "-o", "json", "--no-pager"}
}

// sinceArgs bounds the output to the last 48h without a cursor: first run and fallback when the
// cursor is no longer valid.
func (jm *journalManager) sinceArgs() []string {
	return append(jm.baseStreamArgs(), "--since=-48h")
}

// collectStream reads the new stream (HEAD-mode). If the --after-cursor attempt is rejected
// (typically the cursor was rotated away / is no longer in the journal: journalctl exits != 0) it
// does NOT stay stuck: it falls back ONCE to --since=-48h, so the stream flows again and the server
// deduplicates on [host, cursor]. It returns nil if there is nothing; the caller persists the cursor
// only after a successful push.
func (jm *journalManager) collectStream() ([]journal.Entry, string) {
	entries, cursor, failed := jm.runStream(jm.streamArgs())
	if failed && jm.cursor != "" {
		slog.Warn("journalctl stream: cursor rejected, falling back to --since=-48h", "cursor", jm.cursor)
		entries, cursor, _ = jm.runStream(jm.sinceArgs())
	}
	return entries, cursor
}

// runStream runs journalctl -p N -o json with the given args, stops at the double cap (entries /
// bytes) and returns (entries, cursor of the last consumed line, failed). failed=true ONLY when the
// command exits on its own with code != 0 (command rejected): the HEAD-mode cap and an interrupted
// read are our own kills, with valid data, and do not count as failures.
func (jm *journalManager) runStream(args []string) ([]journal.Entry, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), journalExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, jm.journalctlPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr // stderr captured: a journalctl rejecting the command must NOT end silently
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.Error("journalctl stream: stdout pipe failed", "err", err)
		return nil, jm.cursor, false
	}
	if err := cmd.Start(); err != nil {
		slog.Error("journalctl stream: start failed", "err", err)
		return nil, jm.cursor, false
	}

	entries := make([]journal.Entry, 0, 64)
	lastCursor := jm.cursor
	total := 0
	capped := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), journalScanBufMax)
	for scanner.Scan() {
		line := scanner.Bytes()
		cursor, entry, keep := parseJournalLine(line, jm.priority)
		if cursor == "" {
			continue // malformed line without a cursor: we cannot advance safely
		}
		if keep {
			entries = append(entries, entry)
		}
		lastCursor = cursor
		total += len(line)
		if len(entries) >= journalMaxEntries || total >= journalMaxBytes {
			capped = true
			break
		}
	}

	// Cap reached (HEAD-mode) or interrupted read: the kill is ours, the Wait error is expected and is
	// discarded so as not to leave zombies.
	if capped {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return entries, lastCursor, false
	}
	if serr := scanner.Err(); serr != nil {
		slog.Error("journalctl stream: read interrupted", "err", serr)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return entries, lastCursor, false
	}
	// Clean EOF: journalctl closed on its own. The exit code says whether it rejected the command: the
	// failure MUST be visible, before the outcome and stderr were discarded and the stream stayed stuck
	// for weeks.
	if werr := cmd.Wait(); werr != nil {
		slog.Error("journalctl stream failed",
			"err", werr,
			"stderr", strings.TrimSpace(stderr.String()),
			"args", strings.Join(args, " "))
		return entries, lastCursor, true
	}
	return entries, lastCursor, false
}

// collectSnapshots captures `journalctl -u <unit>` for units that just failed (once per failure,
// tracked in sentSnapshots) and prunes recovered ones from the set (so a new failure re-captures).
func (jm *journalManager) collectSnapshots() map[string]journal.Snapshot {
	if jm.systemd == nil {
		return nil
	}
	failed := jm.systemd.failedUnits()
	current := make(map[string]bool, len(failed))
	snapshots := make(map[string]journal.Snapshot)

	for _, unit := range failed {
		current[unit.WireName] = true
		if jm.sentSnapshots[unit.WireName] {
			continue
		}
		lines := jm.captureUnit(unit.UnitName)
		if len(lines) == 0 {
			continue
		}
		snapshots[unit.WireName] = journal.Snapshot{CapturedAt: time.Now().Unix(), Lines: lines}
		jm.sentSnapshots[unit.WireName] = true
	}

	for wire := range jm.sentSnapshots {
		if !current[wire] {
			delete(jm.sentSnapshots, wire)
		}
	}
	if len(snapshots) == 0 {
		return nil
	}
	return snapshots
}

// captureUnit returns the last `tail` lines of `journalctl -u <unit>` (no priority filter: diagnosis
// also needs the info lines preceding the crash).
func (jm *journalManager) captureUnit(unitName string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), journalExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, jm.journalctlPath,
		"-q", "-u", unitName, "-n", strconv.Itoa(jm.tail), "--no-pager", "-o", "short-iso")
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("journalctl snapshot failed", "unit", unitName, "err", err)
		return nil
	}
	raw := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if line == "" {
			continue
		}
		lines = append(lines, clampRunes(scrubUTF8(line), journalMessageMax))
	}
	return lines
}

func (jm *journalManager) readCursor() string {
	if jm.dataDir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(jm.dataDir, journalCursorFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (jm *journalManager) saveCursor(cursor string) error {
	if jm.dataDir == "" {
		// Without dataDir the cursor lives only in memory: on restart it resumes from --since=-48h and
		// the server deduplicates on [host, cursor]. No error.
		return nil
	}
	return os.WriteFile(filepath.Join(jm.dataDir, journalCursorFile), []byte(cursor), 0o644)
}

// parseJournalLine extracts (cursor, entry, keep) from a `-o json` line. keep=false when the entry
// must be discarded (empty message) but the cursor is still valid → it advances anyway. cursor==""
// only on a malformed line (no __CURSOR).
func parseJournalLine(line []byte, maxPriority int) (string, journal.Entry, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return "", journal.Entry{}, false
	}
	cursor := journalString(raw["__CURSOR"])
	if cursor == "" {
		return "", journal.Entry{}, false
	}
	priority := int(journalInt(raw["PRIORITY"]))
	message := clampRunes(journalMessage(raw["MESSAGE"]), journalMessageMax)
	if message == "" || priority > maxPriority {
		return cursor, journal.Entry{}, false
	}
	entry := journal.Entry{
		Cursor:   cursor,
		Time:     journalInt(raw["__REALTIME_TIMESTAMP"]),
		Priority: priority,
		Unit:     journalUnit(raw),
		Message:  message,
	}
	return cursor, entry, true
}

// journalMessage handles MESSAGE both as a string (common) and as a byte array (journald emits it
// that way when the content is not valid UTF-8). Always scrubbed (null bytes + encoding).
func journalMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return scrubUTF8(s)
	}
	var nums []int
	if err := json.Unmarshal(raw, &nums); err == nil {
		b := make([]byte, len(nums))
		for i, n := range nums {
			b[i] = byte(n)
		}
		return scrubUTF8(string(b))
	}
	return ""
}

func journalUnit(raw map[string]json.RawMessage) string {
	if u := journalString(raw["_SYSTEMD_UNIT"]); u != "" {
		return u
	}
	if u := journalString(raw["SYSLOG_IDENTIFIER"]); u != "" {
		return u
	}
	return "kernel"
}

func journalString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// journalInt: journald emits numbers as STRINGS ("1719912345678901", "3"); falls back to a bare
// number for robustness.
func journalInt(raw json.RawMessage) int64 {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return n
		}
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	return 0
}

// scrubUTF8 removes null bytes (Postgres rejects them) and replaces non-UTF8 sequences with U+FFFD.
func scrubUTF8(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

// clampRunes truncates to max RUNES (not bytes, so as not to split a multibyte character in half).
func clampRunes(s string, max int) string {
	if len(s) <= max { // fast path: byte len <= max ⇒ runes <= max
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func journalEnvInt(key string, def, min, max int) int {
	v, ok := utils.GetEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return def
	}
	return n
}
