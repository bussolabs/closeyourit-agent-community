// Package pusher serializes the wire payload and POSTs it to CloseYourIt
// (POST {endpoint}/api/v1/servers/samples, bearer enrollment token, gzipped JSON).
package pusher

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bussolabs/closeyourit-agent/internal/entities/database"
	"github.com/bussolabs/closeyourit-agent/internal/entities/journal"
	"github.com/bussolabs/closeyourit-agent/internal/entities/smart"
	"github.com/bussolabs/closeyourit-agent/internal/entities/system"
	"github.com/bussolabs/closeyourit-agent/internal/version"
)

// ErrRevoked signals that the server answered 403: the host has been revoked and the agent must
// slow down (long idle) instead of hammering.
var ErrRevoked = errors.New("host revoked (403)")

// ErrHostCredentialRejected signals 401 R401-SERVER-002: the per-machine credential is no longer
// valid and the server asks to present itself again with the enrollment token. It is NOT a host
// revocation (403): it recovers on its own, by dropping the credential and re-enrolling. Without
// telling them apart the agent stayed on the dead token and the host went silent until someone
// logged in to delete credentials.json by hand (CYAG-19).
var ErrHostCredentialRejected = errors.New("per-host credential rejected (401 R401-SERVER-002)")

// ErrRateLimited signals 429: the server asks the agent to slow down. It is never retried within
// the tick — retrying a few seconds later is exactly the hammering the server is pushing back on.
// The wait the server asked for, if any, travels in RateLimitedError.
var ErrRateLimited = errors.New("rate limited by the server (429)")

// RateLimitedError carries the wait read from Retry-After. RetryAfter is zero when the server did
// not say how long (or said it in a form that cannot be read): the next tick simply tries again.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	if e.RetryAfter <= 0 {
		return ErrRateLimited.Error()
	}
	return fmt.Sprintf("%s: retry after %s", ErrRateLimited, e.RetryAfter)
}

func (e *RateLimitedError) Is(target error) bool { return target == ErrRateLimited }

// hostCredentialRejectedCode is the code closeyourit-rails uses for "present yourself again with
// the enrollment token" (servers-sample v1 contract, fixtures/http/auth-cases.json).
const hostCredentialRejectedCode = "R401-SERVER-002"

// Payload is the wire contract with closeyourit-rails (shared fixture
// spec/fixtures/servers/agent_payload.json): compact beszel-derived keys inside `data`,
// readable keys in `host`/`smart`.
type Payload struct {
	Fingerprint  string                     `json:"fingerprint"`
	AgentVersion string                     `json:"agent_version"`
	RecordedAt   time.Time                  `json:"recorded_at"`
	Host         HostInfo                   `json:"host"`
	Data         *system.CombinedData       `json:"data"`
	Smart        map[string]smart.SmartData `json:"smart,omitempty"`
	// Journal is the journald block (native logs): top-level like Smart, omitted on hosts without
	// journalctl or with nothing to send (absent = server-side no-op, backward compatible).
	Journal *journal.Report `json:"journal,omitempty"`
	// Database is the state of the host database (PostgreSQL): optional top-level like Smart/Journal,
	// omitted on hosts without a database (absent = server-side no-op, backward compatible).
	Database *database.Database `json:"database,omitempty"`
}

// HostInfo translates system.Details (which has no short json tags) into stable readable keys.
// RebootRequired/UpdatesAvailable/SecurityUpdatesAvailable come from agent.SystemUpdates (not from
// Details) — main sets them. -1 → "unknown" (non-apt distro); no omitempty so as not to confuse
// -1/0 with the field being absent.
//
// SecurityUpdatesAvailable is the subset of UpdatesAvailable that `apply_security_updates` can
// actually install: without this number the UI shows a total that action will never bring down,
// and the action looks broken even though it worked correctly.
type HostInfo struct {
	Hostname                 string `json:"hostname"`
	Kernel                   string `json:"kernel,omitempty"`
	Cores                    int    `json:"cores,omitempty"`
	Threads                  int    `json:"threads,omitempty"`
	CpuModel                 string `json:"cpu_model,omitempty"`
	OsName                   string `json:"os_name,omitempty"`
	Arch                     string `json:"arch,omitempty"`
	MemoryTotal              uint64 `json:"memory_total,omitempty"`
	RebootRequired           bool   `json:"reboot_required"`
	UpdatesAvailable         int    `json:"updates_available"`
	SecurityUpdatesAvailable int    `json:"security_updates_available"`
}

func HostInfoFromDetails(details system.Details, override string) HostInfo {
	hostname := details.Hostname
	if override != "" {
		hostname = override
	}
	return HostInfo{
		Hostname:    hostname,
		Kernel:      details.Kernel,
		Cores:       details.Cores,
		Threads:     details.Threads,
		CpuModel:    details.CpuModel,
		OsName:      details.OsName,
		Arch:        details.Arch,
		MemoryTotal: details.MemoryTotal,
	}
}

type Pusher struct {
	endpoint  string
	token     string
	client    *http.Client
	hostToken string
}

func (p *Pusher) TakeHostToken() string { token := p.hostToken; p.hostToken = ""; return token }
func (p *Pusher) SetToken(token string) { p.token = token }

func New(endpoint, token string) *Pusher {
	return &Pusher{
		endpoint: endpoint,
		token:    token,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Push sends the payload with 2 retries (backoff with jitter) within the current tick; remaining
// errors are handled by the caller (the next tick retries anyway). A revocation (403) and a rate
// limit (429) are not retried: both ask the agent to back off, not to try again right away.
func (p *Pusher) Push(payload *Payload) error {
	var lastErr error
	for attempt := 0; attempt <= 2; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * (time.Second + time.Duration(rand.IntN(1000))*time.Millisecond)
			time.Sleep(backoff)
		}
		lastErr = p.send(payload)
		if lastErr == nil || errors.Is(lastErr, ErrRevoked) || errors.Is(lastErr, ErrRateLimited) {
			return lastErr
		}
	}
	return lastErr
}

func (p *Pusher) send(payload *Payload) error {
	body, err := encode(payload)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, p.endpoint+"/api/v1/servers/samples", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("User-Agent", version.AppName+"/"+version.Version)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden:
		return ErrRevoked
	case resp.StatusCode == http.StatusTooManyRequests:
		return &RateLimitedError{RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	case resp.StatusCode == http.StatusUnauthorized:
		if errorCode(resp.Body) == hostCredentialRejectedCode {
			return ErrHostCredentialRejected
		}
		return pushError(resp.StatusCode, "")
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var envelope struct {
			Data struct {
				HostToken string `json:"host_token"`
			} `json:"data"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = json.Unmarshal(raw, &envelope)
		p.hostToken = envelope.Data.HostToken
		return nil
	default:
		return pushError(resp.StatusCode, errorCode(resp.Body))
	}
}

// retryAfter reads the Retry-After header in both forms HTTP allows: a delay in seconds or an HTTP
// date. Missing, unreadable or already past values give zero (no wait asked for).
func retryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(math.MaxInt64/time.Second) {
			return math.MaxInt64
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// pushError carries the server code in the message when there is one: in the host logs "HTTP 413"
// alone does not tell a payload over the MiB (R413-SERVER-002) from a malformed body
// (R422-SERVER-004), and they are failures with different remedies.
func pushError(status int, code string) error {
	if code == "" {
		return fmt.Errorf("push failed: HTTP %d", status)
	}
	return fmt.Errorf("push failed: HTTP %d (%s)", status, code)
}

// errorCode extracts the R###-SERVER-### code from the closeyourit-rails error envelope
// ({"error":{"code":…}}). A response without a body or not JSON gives an empty string: no code, no
// special handling.
func errorCode(body io.Reader) string {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(body, 64*1024))
	_ = json.Unmarshal(raw, &envelope)
	return envelope.Error.Code
}

func encode(payload *Payload) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(payload); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}
