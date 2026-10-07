package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRequiresEndpointAndToken(t *testing.T) {
	t.Setenv("CYI_ENDPOINT", "")
	t.Setenv("CYI_TOKEN", "")
	t.Setenv("CYI_TOKEN_FILE", "")

	if _, err := Load(); !errors.Is(err, ErrMissingEndpoint) {
		t.Fatalf("expected ErrMissingEndpoint, got %v", err)
	}

	t.Setenv("CYI_ENDPOINT", "https://www.closeyour.it")
	if _, err := Load(); !errors.Is(err, ErrMissingToken) {
		t.Fatalf("expected ErrMissingToken, got %v", err)
	}
}

func TestLoadDefaultsAndNormalization(t *testing.T) {
	t.Setenv("CYI_ENDPOINT", "https://www.closeyour.it/ ")
	t.Setenv("CYI_TOKEN", " cyi_s_abc ")
	t.Setenv("CYI_INTERVAL", "")
	t.Setenv("CYI_HOST_NAME", "")
	t.Setenv("CYI_TOKEN_FILE", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Endpoint != "https://www.closeyour.it" {
		t.Errorf("endpoint not normalized: %q", cfg.Endpoint)
	}
	if cfg.Token != "cyi_s_abc" {
		t.Errorf("token not trimmed: %q", cfg.Token)
	}
	if cfg.Interval != 60*time.Second {
		t.Errorf("default interval: %v", cfg.Interval)
	}
}

func TestLoadCustomIntervalAndInvalidFallback(t *testing.T) {
	t.Setenv("CYI_ENDPOINT", "https://x")
	t.Setenv("CYI_TOKEN", "tok")
	t.Setenv("CYI_TOKEN_FILE", "")

	t.Setenv("CYI_INTERVAL", "30s")
	cfg, _ := Load()
	if cfg.Interval != 30*time.Second {
		t.Errorf("custom interval: %v", cfg.Interval)
	}

	// below the minimum (1s) or unparsable → default
	t.Setenv("CYI_INTERVAL", "10ms")
	cfg, _ = Load()
	if cfg.Interval != 60*time.Second {
		t.Errorf("an interval below the minimum must fall back to the default: %v", cfg.Interval)
	}

	t.Setenv("CYI_INTERVAL", "boom")
	cfg, _ = Load()
	if cfg.Interval != 60*time.Second {
		t.Errorf("an invalid interval must fall back to the default: %v", cfg.Interval)
	}
}

func TestLoadTokenFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("cyi_s_fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CYI_ENDPOINT", "https://x")
	t.Setenv("CYI_TOKEN", "")
	t.Setenv("CYI_TOKEN_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Token != "cyi_s_fromfile" {
		t.Errorf("token from file: %q", cfg.Token)
	}
}

func TestLoadRejectsCleartextEndpointOutsideLocalhost(t *testing.T) {
	t.Setenv("CYI_TOKEN", "tok")
	t.Setenv("CYI_TOKEN_FILE", "")

	for _, endpoint := range []string{"http://app.closeyour.it", "http://192.0.2.10:3000", "ftp://app.closeyour.it", "app.closeyour.it"} {
		t.Setenv("CYI_ENDPOINT", endpoint)
		if _, err := Load(); !errors.Is(err, ErrInsecureEndpoint) {
			t.Errorf("%q: expected ErrInsecureEndpoint, got %v", endpoint, err)
		}
	}

	for _, endpoint := range []string{"http://localhost:3000", "http://127.0.0.1:3000", "http://[::1]:3000", "https://app.closeyour.it"} {
		t.Setenv("CYI_ENDPOINT", endpoint)
		if _, err := Load(); err != nil {
			t.Errorf("%q: expected no error, got %v", endpoint, err)
		}
	}
}
