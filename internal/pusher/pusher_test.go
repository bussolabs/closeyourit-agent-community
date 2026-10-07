package pusher

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func minimalPayload() *Payload {
	return &Payload{
		Fingerprint:  "abc123",
		AgentVersion: "0.1.0",
		RecordedAt:   time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
		Host:         HostInfo{Hostname: "test"},
	}
}

func TestPushSendsGzippedJSONWithBearer(t *testing.T) {
	var gotAuth, gotEncoding, gotPath string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotEncoding = r.Header.Get("Content-Encoding")
		gotPath = r.URL.Path
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
		}
		raw, _ := io.ReadAll(zr)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	err := New(server.URL, "cyi_s_secret").Push(minimalPayload())

	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if gotPath != "/api/v1/servers/samples" {
		t.Errorf("path: %s", gotPath)
	}
	if gotAuth != "Bearer cyi_s_secret" {
		t.Errorf("auth: %s", gotAuth)
	}
	if gotEncoding != "gzip" {
		t.Errorf("encoding: %s", gotEncoding)
	}
	if gotBody["fingerprint"] != "abc123" {
		t.Errorf("body fingerprint: %v", gotBody["fingerprint"])
	}
}

func TestPush403ReturnsErrRevokedWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	err := New(server.URL, "tok").Push(minimalPayload())

	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected ErrRevoked, got %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("a 403 must not be retried: %d calls", calls.Load())
	}
}

func TestPushCapturesBootstrapHostTokenOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"data":{"accepted":true,"host_token":"cyi_h_secret"}}`))
	}))
	defer server.Close()

	p := New(server.URL, "cyi_s_enrollment")
	if err := p.Push(minimalPayload()); err != nil {
		t.Fatal(err)
	}
	if got := p.TakeHostToken(); got != "cyi_h_secret" {
		t.Fatalf("host token = %q", got)
	}
	if got := p.TakeHostToken(); got != "" {
		t.Fatalf("token must be consumed, got %q", got)
	}
}

func TestPushRetriesServerErrorsThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	err := New(server.URL, "tok").Push(minimalPayload())

	if err != nil {
		t.Fatalf("push after retry: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts, made %d", calls.Load())
	}
}

func TestPushGivesUpAfterRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	err := New(server.URL, "tok").Push(minimalPayload())

	if err == nil {
		t.Fatal("expected an error after the retries")
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 total attempts, made %d", calls.Load())
	}
}

// CYAG-19: a 401 R401-SERVER-002 says "the per-machine credential is no longer valid, present
// yourself again with the enrollment token". Before, the agent read it as a generic error, stayed on
// the dead token and the host went silent forever: the only recovery was deleting credentials.json
// on the host by hand.
func TestPush401RevokedHostTokenIsDistinctFromPermanentRevocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"R401-SERVER-002","message":"credenziale revocata"}}`))
	}))
	defer server.Close()

	err := New(server.URL, "cyi_h_dead").send(minimalPayload())

	if !errors.Is(err, ErrHostCredentialRejected) {
		t.Fatalf("expected ErrHostCredentialRejected, got %v", err)
	}
	if errors.Is(err, ErrRevoked) {
		t.Fatal("a credential to renew is not a host revocation: the agent would go into long idle instead of re-enrolling")
	}
}

// Other 401s (missing or unknown bearer, or revoked enrollment token) remain generic errors: there
// is no per-machine credential to drop there.
func TestPush401GenericDoesNotAskForReenrollment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"R401-SERVER-001","message":"credenziale non riconosciuta"}}`))
	}))
	defer server.Close()

	err := New(server.URL, "cyi_s_wrong").send(minimalPayload())

	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrHostCredentialRejected) {
		t.Fatalf("R401-SERVER-001 must not drop the per-machine credential: %v", err)
	}
}

// A 401 without a readable body must not turn into a random re-enrollment.
func TestPush401WithoutCodeStaysGenericError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	err := New(server.URL, "tok").send(minimalPayload())

	if err == nil || errors.Is(err, ErrHostCredentialRejected) {
		t.Fatalf("expected a generic error, got %v", err)
	}
}

// CYAG-19: a deterministic rejection (413 payload over the MiB, 422 malformed body) is retried
// blindly and only "HTTP 413" is left in the logs. The server code instead says exactly what it
// rejects: without it, the host logs cannot tell an oversized payload from a broken body.
func TestPushErrorCarriesTheServerCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"code":"R413-SERVER-002","message":"payload troppo grande"}}`))
	}))
	defer server.Close()

	err := New(server.URL, "tok").send(minimalPayload())

	if err == nil || !strings.Contains(err.Error(), "R413-SERVER-002") {
		t.Fatalf("the error does not carry the server code: %v", err)
	}
}

// Without a code the message stays as before: no empty parentheses in the logs.
func TestPushErrorWithoutCodeCarriesOnlyTheStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	err := New(server.URL, "tok").send(minimalPayload())

	if err == nil || err.Error() != "push failed: HTTP 500" {
		t.Fatalf("unexpected message: %v", err)
	}
}
