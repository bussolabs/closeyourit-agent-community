package actions

import (
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
