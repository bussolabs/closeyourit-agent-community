// Package utils provides utility functions for the agent.
package utils

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const (
	// envPrefix is the CloseYourIt prefix for the collector configuration env vars.
	envPrefix = "CYI_AGENT_"
	// legacyEnvPrefix is the prefix inherited from the upstream project (Beszel),
	// kept as a deprecated alias so as not to break existing installations.
	legacyEnvPrefix = "BESZEL_AGENT_"
)

// deprecatedEnvWarned tracks the deprecated aliases already reported, so the deprecation
// warning is emitted only once per variable.
var deprecatedEnvWarned sync.Map

// GetEnv looks up an environment variable trying, in order: the CloseYourIt prefix
// "CYI_AGENT_", the deprecated alias "BESZEL_AGENT_" (inherited from Beszel) and finally the
// unprefixed key. When resolution goes through the deprecated alias it emits a one-off warning
// asking to switch to the CloseYourIt name.
func GetEnv(key string) (value string, exists bool) {
	if value, exists = os.LookupEnv(envPrefix + key); exists {
		return value, exists
	}
	if value, exists = os.LookupEnv(legacyEnvPrefix + key); exists {
		warnDeprecatedEnv(legacyEnvPrefix+key, envPrefix+key)
		return value, exists
	}
	return os.LookupEnv(key)
}

// warnDeprecatedEnv reports the use of a deprecated alias, only once per variable.
func warnDeprecatedEnv(oldName, newName string) {
	if _, alreadyWarned := deprecatedEnvWarned.LoadOrStore(oldName, struct{}{}); alreadyWarned {
		return
	}
	slog.Warn("deprecated environment variable: rename it with the CloseYourIt prefix before the alias is removed",
		"deprecated", oldName, "use", newName)
}

// BytesToMegabytes converts bytes to megabytes and rounds to two decimal places.
func BytesToMegabytes(b float64) float64 {
	return TwoDecimals(b / 1048576)
}

// BytesToGigabytes converts bytes to gigabytes and rounds to two decimal places.
func BytesToGigabytes(b uint64) float64 {
	return TwoDecimals(float64(b) / 1073741824)
}

// TwoDecimals rounds a float64 value to two decimal places.
func TwoDecimals(value float64) float64 {
	return math.Round(value*100) / 100
}

// func RoundFloat(val float64, precision uint) float64 {
//     ratio := math.Pow(10, float64(precision))
//     return math.Round(val*ratio) / ratio
// }

// ReadStringFile returns trimmed file contents or empty string on error.
func ReadStringFile(path string) string {
	content, _ := ReadStringFileOK(path)
	return content
}

// ReadStringFileOK returns trimmed file contents and read success.
func ReadStringFileOK(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// ReadStringFileLimited reads a file into a string with a maximum size (in bytes) to avoid
// allocating large buffers and potential panics with pseudo-files when the size is misreported.
func ReadStringFileLimited(path string, maxSize int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, maxSize)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return "", err
	}
	if n < 0 {
		return "", fmt.Errorf("%s returned negative bytes: %d", path, n)
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

// FileExists reports whether the given path exists.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ReadUintFile parses a decimal uint64 value from a file.
func ReadUintFile(path string) (uint64, bool) {
	raw, ok := ReadStringFileOK(path)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// LookPathHomebrew is like exec.LookPath but also checks Homebrew paths.
func LookPathHomebrew(file string) (string, error) {
	foundPath, lookPathErr := exec.LookPath(file)
	if lookPathErr == nil {
		return foundPath, nil
	}
	var homebrewPath string
	switch runtime.GOOS {
	case "darwin":
		homebrewPath = filepath.Join("/opt", "homebrew", "bin", file)
	case "linux":
		homebrewPath = filepath.Join("/home", "linuxbrew", ".linuxbrew", "bin", file)
	}
	if homebrewPath != "" {
		if _, err := os.Stat(homebrewPath); err == nil {
			return homebrewPath, nil
		}
	}
	return "", lookPathErr
}
