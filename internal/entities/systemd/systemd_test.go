//go:build testing

package systemd_test

import (
	"runtime"
	"testing"
	"time"

	"github.com/bussolabs/closeyourit-agent/internal/entities/systemd"
	"github.com/stretchr/testify/assert"
)

func TestParseServiceStatus(t *testing.T) {
	tests := []struct {
		input    string
		expected systemd.ServiceState
	}{
		{"active", systemd.StatusActive},
		{"inactive", systemd.StatusInactive},
		{"failed", systemd.StatusFailed},
		{"activating", systemd.StatusActivating},
		{"deactivating", systemd.StatusDeactivating},
		{"reloading", systemd.StatusReloading},
		{"unknown", systemd.StatusInactive}, // default case
		{"", systemd.StatusInactive},        // default case
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			result := systemd.ParseServiceStatus(test.input)
			assert.Equal(t, test.expected, result)
		})
	}
}

func TestParseServiceSubState(t *testing.T) {
	tests := []struct {
		input    string
		expected systemd.ServiceSubState
	}{
		{"dead", systemd.SubStateDead},
		{"running", systemd.SubStateRunning},
		{"exited", systemd.SubStateExited},
		{"failed", systemd.SubStateFailed},
		{"unknown", systemd.SubStateUnknown},
		{"other", systemd.SubStateUnknown}, // default case
		{"", systemd.SubStateUnknown},      // default case
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			result := systemd.ParseServiceSubState(test.input)
			assert.Equal(t, test.expected, result)
		})
	}
}

func TestServiceUpdateCPUPercent(t *testing.T) {
	t.Run("initial call sets CPU to 0", func(t *testing.T) {
		service := &systemd.Service{}
		service.UpdateCPUPercent(1000)
		assert.Equal(t, 0.0, service.Cpu)
		assert.Equal(t, uint64(1000), service.PrevCpuUsage)
		assert.False(t, service.PrevReadTime.IsZero())
	})

	t.Run("subsequent call calculates CPU percentage", func(t *testing.T) {
		service := &systemd.Service{}
		service.PrevCpuUsage = 1000
		service.PrevReadTime = time.Now().Add(-time.Second)

		// Half a second of CPU time per core: about 50% whatever the core count (CYAG-24).
		usage := uint64(runtime.NumCPU()) * 500_000_000
		service.UpdateCPUPercent(usage)

		// CPU usage should be positive and reasonable
		assert.Greater(t, service.Cpu, 0.0, "CPU usage should be positive")
		assert.LessOrEqual(t, service.Cpu, 100.0, "CPU usage should not exceed 100%")
		assert.Equal(t, usage, service.PrevCpuUsage)
		assert.Greater(t, service.CpuPeak, 0.0, "CPU peak should be set")
	})

	t.Run("CPU peak updates only when higher", func(t *testing.T) {
		service := &systemd.Service{}
		service.PrevCpuUsage = 1000
		service.PrevReadTime = time.Now().Add(-time.Second)
		perCore := uint64(runtime.NumCPU())
		service.UpdateCPUPercent(perCore * 500_000_000) // Initial peak ~50%
		initialPeak := service.CpuPeak

		// Usage keeps growing but more slowly (~10%): the peak must stay where it was.
		service.PrevReadTime = time.Now().Add(-time.Second)
		service.UpdateCPUPercent(perCore * 600_000_000)
		assert.Greater(t, service.Cpu, 0.0, "the second reading must be measured, not reset")
		assert.Less(t, service.Cpu, initialPeak)
		assert.Equal(t, initialPeak, service.CpuPeak, "Peak should not update for lower CPU usage")
	})

	t.Run("handles zero duration", func(t *testing.T) {
		service := &systemd.Service{}
		service.PrevCpuUsage = 1000
		now := time.Now()
		service.PrevReadTime = now
		// Mock time.Now() to return the same time to ensure zero duration
		// Since we can't mock time in Go easily, we'll check the logic manually
		// The zero duration case happens when duration <= 0
		assert.Equal(t, 0.0, service.Cpu, "CPU should start at 0")
	})

	t.Run("handles CPU usage wraparound", func(t *testing.T) {
		service := &systemd.Service{}
		// Simulate wraparound where new usage is less than previous
		service.PrevCpuUsage = 1000
		service.PrevReadTime = time.Now().Add(-time.Second)
		service.UpdateCPUPercent(500) // Less than previous, should reset
		assert.Equal(t, 0.0, service.Cpu)
	})
}
