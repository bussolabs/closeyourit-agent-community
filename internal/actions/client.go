package actions

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
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

// Permanent reports whether the server refused the request for good: a 4xx other than an
// expired credential (401, recoverable after re-registration), a timeout (408) or a rate
// limit (429). Retrying a permanent rejection would only repeat it forever.
func (e *HTTPError) Permanent() bool {
	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return e.StatusCode >= 400 && e.StatusCode < 500
}
