// Package journal describes the journald block of the wire payload (native server logs): a stream
// of priority <= err/crit (cursor-based) + journalctl snapshots of failed systemd units. It is
// top-level in the Payload (like smart), outside the beszel-derived `data`, and absent when the host
// has no journalctl (e.g. Alpine Docker image) or there is nothing to send.
package journal

// Report is the `journal` block of the payload. The compact keys match the server contract
// (closeyourit-rails Servers::Ingest::Normalize + fixture agent_payload.json).
type Report struct {
	Entries   []Entry             `json:"entries,omitempty"`
	Snapshots map[string]Snapshot `json:"snapshots,omitempty"`
}

// Entry is a journald line. Time is __REALTIME_TIMESTAMP in epoch microseconds; Priority is the
// syslog PRIORITY (0..7); Unit is _SYSTEMD_UNIT || SYSLOG_IDENTIFIER || "kernel"; Cursor is __CURSOR
// (server-side idempotency on [host, cursor]).
type Entry struct {
	Cursor   string `json:"c"`
	Time     int64  `json:"t"`
	Priority int    `json:"p"`
	Unit     string `json:"u,omitempty"`
	Message  string `json:"m"`
}

// Snapshot is the tail of `journalctl -u <unit>` for a failed unit. CapturedAt is in epoch seconds.
// The KEY in the Snapshots map is the unit wire name (== systemd `n` field), so the server attaches
// the snapshot to the matching systemd row.
type Snapshot struct {
	CapturedAt int64    `json:"t"`
	Lines      []string `json:"lines"`
}
