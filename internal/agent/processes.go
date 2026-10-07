package agent

import (
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/bussolabs/closeyourit-agent/internal/agent/utils"
	"github.com/bussolabs/closeyourit-agent/internal/entities/system"
	"github.com/shirou/gopsutil/v4/process"
)

// defaultProcessTopN is how many processes enter each ranking (cpu, memory).
const defaultProcessTopN = 10

// processManager collects the top-N processes by CPU and memory. CPU% is computed on the delta of
// CPU times between two pushes (like top), not on the process lifetime average: what matters when
// an alert fires is who is consuming NOW. On the first round there is no delta and CPU stays 0: the
// memory ranking works right away, the CPU one from the second push.
type processManager struct {
	topN          int
	prevCpuTimes  map[int32]float64
	prevCollected time.Time
}

func newProcessManager() *processManager {
	topN := defaultProcessTopN
	if raw, ok := utils.GetEnv("PROCESSES_TOP_N"); ok {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			topN = parsed
		} else {
			slog.Warn("Invalid PROCESSES_TOP_N", "value", raw)
		}
	}
	return &processManager{topN: topN, prevCpuTimes: map[int32]float64{}}
}

// getStats returns the deduplicated top-N. Per-process errors (process died between list and read,
// permissions) are silently ignored: the list is best-effort by construction.
func (pm *processManager) getStats() []system.ProcessStat {
	procs, err := process.Processes()
	if err != nil {
		slog.Debug("Processes", "err", err)
		return nil
	}

	now := time.Now()
	elapsed := now.Sub(pm.prevCollected).Seconds()
	firstRun := pm.prevCollected.IsZero()
	nextCpuTimes := make(map[int32]float64, len(procs))

	stats := make([]system.ProcessStat, 0, len(procs))
	for _, proc := range procs {
		times, err := proc.Times()
		if err != nil {
			continue
		}
		total := times.User + times.System
		nextCpuTimes[proc.Pid] = total

		cpuPct := 0.0
		if prev, seen := pm.prevCpuTimes[proc.Pid]; seen && !firstRun && elapsed > 0 && total >= prev {
			cpuPct = utils.TwoDecimals((total - prev) / elapsed * 100)
		}

		var memBytes uint64
		if mem, err := proc.MemoryInfo(); err == nil && mem != nil {
			memBytes = mem.RSS
		}

		name, err := proc.Name()
		if err != nil || name == "" {
			continue
		}

		stats = append(stats, system.ProcessStat{
			Pid: proc.Pid, Name: name, CpuPct: cpuPct, MemBytes: memBytes,
		})
	}

	pm.prevCpuTimes = nextCpuTimes
	pm.prevCollected = now

	return selectTopProcesses(stats, pm.topN)
}

// selectTopProcesses merges the top-N by CPU and the top-N by memory, deduplicating by pid.
// The final order is by descending CPU (on ties, by memory): the typical question is "who is eating
// the machine", and CPU is the most volatile answer.
func selectTopProcesses(stats []system.ProcessStat, topN int) []system.ProcessStat {
	if topN <= 0 || len(stats) == 0 {
		return nil
	}

	byCpu := slices.SortedStableFunc(slices.Values(stats), func(a, b system.ProcessStat) int {
		switch {
		case a.CpuPct > b.CpuPct:
			return -1
		case a.CpuPct < b.CpuPct:
			return 1
		default:
			return 0
		}
	})
	byMem := slices.SortedStableFunc(slices.Values(stats), func(a, b system.ProcessStat) int {
		switch {
		case a.MemBytes > b.MemBytes:
			return -1
		case a.MemBytes < b.MemBytes:
			return 1
		default:
			return 0
		}
	})

	seen := make(map[int32]struct{}, topN*2)
	selected := make([]system.ProcessStat, 0, topN*2)
	pick := func(list []system.ProcessStat) {
		for _, stat := range list[:min(topN, len(list))] {
			if _, dup := seen[stat.Pid]; dup {
				continue
			}
			seen[stat.Pid] = struct{}{}
			selected = append(selected, stat)
		}
	}
	pick(byCpu)
	pick(byMem)

	slices.SortStableFunc(selected, func(a, b system.ProcessStat) int {
		switch {
		case a.CpuPct > b.CpuPct:
			return -1
		case a.CpuPct < b.CpuPct:
			return 1
		case a.MemBytes > b.MemBytes:
			return -1
		case a.MemBytes < b.MemBytes:
			return 1
		default:
			return 0
		}
	})
	return selected
}
