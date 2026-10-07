// Package config reads the agent configuration from the CYI_* env vars.
// The env vars of the collectors derived from Beszel (EXCLUDE_CONTAINERS, SENSORS, SMART_*, DOCKER_*,
// MEM_CALC, EXTRA_FILESYSTEMS, …) stay unchanged and are read directly by the collectors.
package config

import (
	"errors"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Endpoint string        // CYI_ENDPOINT — CloseYourIt base URL (e.g. https://app.closeyour.it)
	Token    string        // CYI_TOKEN — universal enrollment token (cyi_s_…)
	Interval time.Duration // CYI_INTERVAL — push cadence (default 60s)
	HostName string        // CYI_HOST_NAME — override of the reported name (default: hostname)
}

const defaultInterval = 60 * time.Second

var (
	ErrMissingEndpoint = errors.New("CYI_ENDPOINT missing")
	ErrMissingToken    = errors.New("CYI_TOKEN missing")
	// ErrInsecureEndpoint: the token travels in every push, so a cleartext server is accepted
	// only on the local machine (development against a Rails server on localhost).
	ErrInsecureEndpoint = errors.New("CYI_ENDPOINT must use https (http is allowed only for localhost)")
)

// Load validates and returns the configuration. TOKEN also accepts a file (CYI_TOKEN_FILE).
func Load() (Config, error) {
	cfg := Config{
		Endpoint: strings.TrimRight(strings.TrimSpace(os.Getenv("CYI_ENDPOINT")), "/"),
		Token:    strings.TrimSpace(os.Getenv("CYI_TOKEN")),
		Interval: defaultInterval,
		HostName: strings.TrimSpace(os.Getenv("CYI_HOST_NAME")),
	}

	if cfg.Token == "" {
		if path := strings.TrimSpace(os.Getenv("CYI_TOKEN_FILE")); path != "" {
			if raw, err := os.ReadFile(path); err == nil {
				cfg.Token = strings.TrimSpace(string(raw))
			}
		}
	}

	if raw := strings.TrimSpace(os.Getenv("CYI_INTERVAL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed >= time.Second {
			cfg.Interval = parsed
		}
	}

	if cfg.Endpoint == "" {
		return cfg, ErrMissingEndpoint
	}
	if !endpointIsSecure(cfg.Endpoint) {
		return cfg, ErrInsecureEndpoint
	}
	if cfg.Token == "" {
		return cfg, ErrMissingToken
	}
	return cfg, nil
}

func endpointIsSecure(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "https":
		return true
	case "http":
		host := parsed.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}
