package actions

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// CYAG-9 regression: the action client had a 35-minute Timeout, so a single claim towards an
// unresponsive network blocked the loop for half an hour. Now it must be short.
func TestActionClientHasShortTimeout(t *testing.T) {
	timeout := NewClient("https://example.test", "cyi_h_token").http.Timeout
	if timeout < 5*time.Second || timeout > 30*time.Second {
		t.Fatalf("action timeout outside the expected range (~15s): %v", timeout)
	}
}

// On an unresponsive network the claim must stop within the timeout, not hang.
func TestClaimStopsOnUnresponsiveNetwork(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-blocked // does not answer until the test unblocks it
	}))
	defer srv.Close()
	defer close(blocked)

	c := &Client{endpoint: srv.URL, token: "cyi_h_token", http: &http.Client{Timeout: 40 * time.Millisecond}}
	start := time.Now()
	if _, err := c.Claim(); err == nil {
		t.Fatal("expected a timeout error on an unresponsive network")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("the timeout did not stop the request in time: %v", elapsed)
	}
}

// CYAG-27: only the three answers that never change on retry are a permanent rejection of a
// result; every other answer from the panel stays transient and the result is retried.
func TestCompleteTellsPermanentFromTransientRejections(t *testing.T) {
	cases := []struct {
		status    int
		permanent bool
	}{
		{http.StatusNotFound, true},            // unknown action (e.g. host added again)
		{http.StatusGone, true},                // action gone
		{http.StatusUnprocessableEntity, true}, // transition not allowed
		{http.StatusUnauthorized, false},       // credential reloaded on every round
		{http.StatusForbidden, false},
		{http.StatusBadRequest, false},
		{http.StatusRequestTimeout, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut || r.URL.Path != "/api/v1/servers/actions/act_1" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(tc.status)
		}))

		err := NewClient(srv.URL, "cyi_h_token").Complete("act_1", Result{Status: "succeeded"})
		srv.Close()

		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != tc.status {
			t.Fatalf("HTTP %d: expected an HTTPError with the status code, got %v", tc.status, err)
		}
		if got := rejectedForGood(err); got != tc.permanent {
			t.Fatalf("HTTP %d: rejectedForGood = %v, expected %v", tc.status, got, tc.permanent)
		}
	}
}

// An accepted result is no error at all.
func TestCompleteAcceptedResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"accepted":true}}`))
	}))
	defer srv.Close()

	if err := NewClient(srv.URL, "cyi_h_token").Complete("act_1", Result{Status: "succeeded"}); err != nil {
		t.Fatalf("accepted result reported as an error: %v", err)
	}
}

// A network failure is never a permanent rejection: the result must be kept and retried.
func TestNetworkFailureIsNotRejectedForGood(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // nothing listens any more

	err := NewClient(srv.URL, "cyi_h_token").Complete("act_1", Result{Status: "succeeded"})
	if err == nil || rejectedForGood(err) {
		t.Fatalf("network failure must be a transient error, got %v", err)
	}
}
