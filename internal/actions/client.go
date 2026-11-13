package actions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Action struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Result struct {
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Client struct {
	endpoint, token string
	http            *http.Client
}

// actionClientTimeout is deliberately short: claim and complete are quick HTTP calls.
// A long timeout would let a single claim on a dead network block the action loop;
// running the action has its own timeout (see Executor), not this client.
const actionClientTimeout = 15 * time.Second

// maxClaimResponseBytes caps how much of a claim answer is read, like the push does: an action is a
// few hundred bytes, so a bigger answer is broken and must not grow the agent's memory.
const maxClaimResponseBytes = 64 * 1024

func NewClient(endpoint, token string) *Client {
	return &Client{endpoint: endpoint, token: token, http: &http.Client{Timeout: actionClientTimeout}}
}

func (c *Client) Claim() (*Action, error) {
	req, _ := http.NewRequest(http.MethodPost, c.endpoint+"/api/v1/servers/action_claims", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claim: HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Data Action `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxClaimResponseBytes)).Decode(&envelope); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

func (c *Client) Complete(id string, result Result) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodPut, c.endpoint+"/api/v1/servers/actions/"+id, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Op: "complete", StatusCode: resp.StatusCode}
	}
	return nil
}

// HTTPError is a server answer outside 2xx. It keeps the status code so the caller can tell a
// transient failure (worth retrying) from a permanent rejection (see Permanent).
type HTTPError struct {
	Op         string
	StatusCode int
}

func (e *HTTPError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.Op, e.StatusCode) }

// Permanent reports whether the server refused the result for good, with one of the three
// answers that never change on retry: unknown action (404, e.g. the host was removed and added
// again to the panel), action gone (410) and state transition not allowed (422). Every other
// answer — expired credential (401, reloaded on every round), rate limit, server error,
// malformed request — stays transient: dropping a result on a transient failure would leave
// the panel without knowing how the action went.
func (e *HTTPError) Permanent() bool {
	switch e.StatusCode {
	case http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// rejectedForGood reports whether err is a permanent rejection from the server (see Permanent).
// Network errors and transient HTTP answers are not: the result is kept and retried.
func rejectedForGood(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.Permanent()
}
