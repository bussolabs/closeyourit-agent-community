package utils

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTwoDecimals(t *testing.T) {
	tests := []struct {
		name     string
		input    float64
		expected float64
	}{
		{"round down", 1.234, 1.23},
		{"round half up", 1.235, 1.24}, // math.Round rounds half up
		{"no rounding needed", 1.23, 1.23},
		{"negative number", -1.235, -1.24}, // math.Round rounds half up (more negative)
		{"zero", 0.0, 0.0},
		{"large number", 123.456, 123.46}, // rounds 5 up
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TwoDecimals(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBytesToMegabytes(t *testing.T) {
	tests := []struct {
		name     string
		input    float64
		expected float64
	}{
		{"1 MB", 1048576, 1.0},
		{"512 KB", 524288, 0.5},
		{"zero", 0, 0},
		{"large value", 1073741824, 1024}, // 1 GB = 1024 MB
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BytesToMegabytes(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBytesToGigabytes(t *testing.T) {
	tests := []struct {
		name     string
		input    uint64
		expected float64
	}{
		{"1 GB", 1073741824, 1.0},
		{"512 MB", 536870912, 0.5},
		{"0 GB", 0, 0},
		{"2 GB", 2147483648, 2.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BytesToGigabytes(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFileFunctions(t *testing.T) {
	tmpDir := t.TempDir()
	testFilePath := filepath.Join(tmpDir, "test.txt")
	testContent := "hello world"

	// Test FileExists (false)
	assert.False(t, FileExists(testFilePath))

	// Test ReadStringFileOK (false)
	content, ok := ReadStringFileOK(testFilePath)
	assert.False(t, ok)
	assert.Empty(t, content)

	// Test ReadStringFile (empty)
	assert.Empty(t, ReadStringFile(testFilePath))

	// Write file
	err := os.WriteFile(testFilePath, []byte(testContent+"\n "), 0644)
	assert.NoError(t, err)

	// Test FileExists (true)
	assert.True(t, FileExists(testFilePath))

	// Test ReadStringFileOK (true)
	content, ok = ReadStringFileOK(testFilePath)
	assert.True(t, ok)
	assert.Equal(t, testContent, content)

	// Test ReadStringFile (content)
	assert.Equal(t, testContent, ReadStringFile(testFilePath))
}

func TestReadUintFile(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("valid uint", func(t *testing.T) {
		path := filepath.Join(tmpDir, "uint.txt")
		os.WriteFile(path, []byte(" 12345\n"), 0644)
		val, ok := ReadUintFile(path)
		assert.True(t, ok)
		assert.Equal(t, uint64(12345), val)
	})

	t.Run("invalid uint", func(t *testing.T) {
		path := filepath.Join(tmpDir, "invalid.txt")
		os.WriteFile(path, []byte("abc"), 0644)
		val, ok := ReadUintFile(path)
		assert.False(t, ok)
		assert.Equal(t, uint64(0), val)
	})

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(tmpDir, "missing.txt")
		val, ok := ReadUintFile(path)
		assert.False(t, ok)
		assert.Equal(t, uint64(0), val)
	})
}

func TestGetEnv(t *testing.T) {
	key := "TEST_VAR"

	t.Run("the CloseYourIt CYI_AGENT_ prefix takes precedence", func(t *testing.T) {
		t.Setenv("CYI_AGENT_"+key, "cyi_val")
		t.Setenv("BESZEL_AGENT_"+key, "legacy_val")
		t.Setenv(key, "unprefixed_val")

		val, exists := GetEnv(key)
		assert.True(t, exists)
		assert.Equal(t, "cyi_val", val)
	})

	t.Run("the deprecated BESZEL_AGENT_ alias still works on its own", func(t *testing.T) {
		t.Setenv("BESZEL_AGENT_"+key, "legacy_val")

		val, exists := GetEnv(key)
		assert.True(t, exists)
		assert.Equal(t, "legacy_val", val)
	})

	t.Run("the deprecated alias takes precedence over the unprefixed key", func(t *testing.T) {
		t.Setenv("BESZEL_AGENT_"+key, "legacy_val")
		t.Setenv(key, "unprefixed_val")

		val, exists := GetEnv(key)
		assert.True(t, exists)
		assert.Equal(t, "legacy_val", val)
	})

	t.Run("only the unprefixed key exists", func(t *testing.T) {
		t.Setenv(key, "unprefixed_val")

		val, exists := GetEnv(key)
		assert.True(t, exists)
		assert.Equal(t, "unprefixed_val", val)
	})

	t.Run("no variable set", func(t *testing.T) {
		val, exists := GetEnv(key)
		assert.False(t, exists)
		assert.Empty(t, val)
	})
}

func TestGetEnvDeprecationWarning(t *testing.T) {
	t.Run("the deprecated alias emits a warning with the old and new name", func(t *testing.T) {
		buf := captureSlogWarnings(t)

		// Dedicated key to avoid the global warning deduplication across tests.
		legacyKey := "TEST_DEPRECATION_WARN"
		t.Setenv("BESZEL_AGENT_"+legacyKey, "x")

		val, exists := GetEnv(legacyKey)
		assert.True(t, exists)
		assert.Equal(t, "x", val)

		out := buf.String()
		assert.Contains(t, out, "BESZEL_AGENT_"+legacyKey)
		assert.Contains(t, out, "CYI_AGENT_"+legacyKey)
	})

	t.Run("the warning is emitted only once per variable", func(t *testing.T) {
		buf := captureSlogWarnings(t)

		legacyKey := "TEST_DEPRECATION_ONCE"
		t.Setenv("BESZEL_AGENT_"+legacyKey, "x")

		GetEnv(legacyKey)
		GetEnv(legacyKey)
		GetEnv(legacyKey)

		occurrences := bytes.Count(buf.Bytes(), []byte("BESZEL_AGENT_"+legacyKey))
		assert.Equal(t, 1, occurrences)
	})

	t.Run("the CYI_AGENT_ prefix emits no warning", func(t *testing.T) {
		buf := captureSlogWarnings(t)

		key := "TEST_NO_WARN"
		t.Setenv("CYI_AGENT_"+key, "x")

		GetEnv(key)

		assert.Empty(t, buf.String())
	})
}

// captureSlogWarnings redirects the default logger to a buffer for the duration of the test.
func captureSlogWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}
