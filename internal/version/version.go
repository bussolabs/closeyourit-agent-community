// Package version exposes the agent identity and version (injectable at build time via ldflags).
package version

var (
	// Version is overridden in releases with: -ldflags "-X .../internal/version.Version=vX.Y.Z"
	Version = "0.9.1"
	AppName = "closeyourit-agent"
)
