//go:build testing

package agent

import (
	"testing"
	"time"

	"github.com/bussolabs/closeyourit-agent/internal/entities/system"
	"github.com/stretchr/testify/assert"
)

func TestSelectTopProcessesUnionAndOrder(t *testing.T) {
	stats := []system.ProcessStat{
		{Pid: 1, Name: "idle-fat", CpuPct: 0.1, MemBytes: 9_000},
		{Pid: 2, Name: "busy-slim", CpuPct: 90, MemBytes: 100},
		{Pid: 3, Name: "both", CpuPct: 50, MemBytes: 5_000},
		{Pid: 4, Name: "quiet", CpuPct: 0.2, MemBytes: 200},
	}

	top := selectTopProcesses(stats, 2)

	// Union: top-2 cpu (2, 3) + top-2 mem (1, 3) dedup → 3 items, ordered by cpu.
	assert.Len(t, top, 3)
	assert.Equal(t, int32(2), top[0].Pid)
	assert.Equal(t, int32(3), top[1].Pid)
	assert.Equal(t, int32(1), top[2].Pid)
}

func TestSelectTopProcessesEdgeCases(t *testing.T) {
	assert.Nil(t, selectTopProcesses(nil, 10))
	assert.Nil(t, selectTopProcesses([]system.ProcessStat{{Pid: 1}}, 0))

	one := selectTopProcesses([]system.ProcessStat{{Pid: 1, Name: "solo"}}, 10)
	assert.Len(t, one, 1)
}

func TestProcessManagerFirstRunHasZeroCpu(t *testing.T) {
	pm := newProcessManager()

	first := pm.getStats()
	if len(first) == 0 {
		t.Skip("no readable process in this environment")
	}
	for _, stat := range first {
		assert.Zero(t, stat.CpuPct, "on the first round there is no delta: cpu must be 0 (%s)", stat.Name)
	}

	// From the second round cpu can be > 0 and the list stays capped at twice topN.
	time.Sleep(50 * time.Millisecond)
	second := pm.getStats()
	assert.LessOrEqual(t, len(second), pm.topN*2)
}
