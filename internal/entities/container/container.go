package container

import (
	"strings"
	"time"
)

// Docker container info from /containers/json
type ApiInfo struct {
	Id      string
	IdShort string
	Names   []string
	Status  string
	State   string
	Image   string
	Health  struct {
		Status string
		// FailingStreak int
	}
	Ports []struct {
		// PrivatePort uint16
		PublicPort uint16
		IP         string
		// Type        string
	}
	Labels map[string]string
	// ImageID string
	// Command string
	// Created int64
	// SizeRw     int64 `json:",omitempty"`
	// SizeRootFs int64 `json:",omitempty"`
	// HostConfig struct {
	// 	NetworkMode string            `json:",omitempty"`
	// 	Annotations map[string]string `json:",omitempty"`
	// }
	// NetworkSettings *SummaryNetworkSettings
	// Mounts          []MountPoint
}

// Docker container resources from /containers/{id}/stats
type ApiStats struct {
	Read        time.Time `json:"read"`               // Time of stats generation
	NumProcs    uint32    `json:"num_procs,omitzero"` // Windows specific, not populated on Linux.
	Networks    map[string]NetworkStats
	CPUStats    CPUStats    `json:"cpu_stats"`
	MemoryStats MemoryStats `json:"memory_stats"`
}

// Docker system info from /info API endpoint
type HostInfo struct {
	OperatingSystem string `json:"OperatingSystem"`
	KernelVersion   string `json:"KernelVersion"`
	NCPU            int    `json:"NCPU"`
	MemTotal        uint64 `json:"MemTotal"`
}

func (s *ApiStats) CalculateCpuPercentLinux(prevCpuContainer uint64, prevCpuSystem uint64) float64 {
	cpuDelta := s.CPUStats.CPUUsage.TotalUsage - prevCpuContainer
	systemDelta := s.CPUStats.SystemUsage - prevCpuSystem

	// Avoid division by zero and handle first run case
	if systemDelta == 0 || prevCpuContainer == 0 {
		return 0.0
	}

	return float64(cpuDelta) / float64(systemDelta) * 100.0
}

// from: https://github.com/docker/cli/blob/master/cli/command/container/stats_helpers.go#L185
func (s *ApiStats) CalculateCpuPercentWindows(prevCpuUsage uint64, prevRead time.Time) float64 {
	// Max number of 100ns intervals between the previous time read and now
	possIntervals := uint64(s.Read.Sub(prevRead).Nanoseconds())
	possIntervals /= 100                // Convert to number of 100ns intervals
	possIntervals *= uint64(s.NumProcs) // Multiple by the number of processors

	// Intervals used
	intervalsUsed := s.CPUStats.CPUUsage.TotalUsage - prevCpuUsage

	// Percentage avoiding divide-by-zero
	if possIntervals > 0 {
		return float64(intervalsUsed) / float64(possIntervals) * 100.0
	}
	return 0.00
}

type CPUStats struct {
	// CPU Usage. Linux and Windows.
	CPUUsage CPUUsage `json:"cpu_usage"`
	// System Usage. Linux only.
	SystemUsage uint64 `json:"system_cpu_usage,omitempty"`
}

type CPUUsage struct {
	// Total CPU time consumed.
	// Units: nanoseconds (Linux)
	// Units: 100's of nanoseconds (Windows)
	TotalUsage uint64 `json:"total_usage"`
}

type MemoryStats struct {
	// current res_counter usage for memory
	Usage uint64 `json:"usage,omitempty"`
	// all the stats exported via memory.stat.
	Stats MemoryStatsStats `json:"stats"`
	// private working set (Windows only)
	PrivateWorkingSet uint64 `json:"privateworkingset,omitempty"`
}

type MemoryStatsStats struct {
	Cache        uint64 `json:"cache,omitempty"`
	InactiveFile uint64 `json:"inactive_file,omitempty"`
}

type NetworkStats struct {
	// Bytes received. Windows and Linux.
	RxBytes uint64 `json:"rx_bytes"`
	// Bytes sent. Windows and Linux.
	TxBytes uint64 `json:"tx_bytes"`
}

type prevNetStats struct {
	Sent uint64
	Recv uint64
}

type DockerHealth = uint8

const (
	DockerHealthNone DockerHealth = iota
	DockerHealthStarting
	DockerHealthHealthy
	DockerHealthUnhealthy
)

var DockerHealthStrings = map[string]DockerHealth{
	"none":      DockerHealthNone,
	"starting":  DockerHealthStarting,
	"healthy":   DockerHealthHealthy,
	"unhealthy": DockerHealthUnhealthy,
}

// Docker container stats
type Stats struct {
	Name        string    `json:"n" cbor:"0,keyasint"`
	Cpu         float64   `json:"c" cbor:"1,keyasint"`
	Mem         float64   `json:"m" cbor:"2,keyasint"`
	NetworkSent float64   `json:"ns,omitzero" cbor:"3,keyasint,omitzero"` // deprecated 0.18.3 (MB) - keep field for old agents/records
	NetworkRecv float64   `json:"nr,omitzero" cbor:"4,keyasint,omitzero"` // deprecated 0.18.3 (MB) - keep field for old agents/records
	Bandwidth   [2]uint64 `json:"b,omitzero" cbor:"9,keyasint,omitzero"`  // [sent bytes, recv bytes]

	Health DockerHealth `json:"h" cbor:"5,keyasint"`
	Status string       `json:"st" cbor:"6,keyasint"`
	Id     string       `json:"id,omitempty" cbor:"7,keyasint"`
	Image  string       `json:"img" cbor:"8,keyasint"`
	Ports  string       `json:"-" cbor:"10,keyasint"`
	// IdleManaged: an idle-sleep manager may stop this container on purpose, so the stopped
	// state is not a failure. Collecting with all=1 keeps the label even when stopped.
	IdleManaged bool `json:"im,omitzero" cbor:"11,keyasint,omitzero"`
	// Running makes the state explicit also for stopped containers, which are collected with
	// /containers/json?all=1. RestartCount, StartedAt and OOMKilled come from container inspect.
	Running      bool   `json:"run" cbor:"12,keyasint"`
	RestartCount int    `json:"rc,omitempty" cbor:"13,keyasint,omitempty"`
	StartedAt    string `json:"sa,omitempty" cbor:"14,keyasint,omitempty"`
	OOMKilled    bool   `json:"oom,omitempty" cbor:"15,keyasint,omitempty"`
	// PrevCpu     [2]uint64    `json:"-"`
	CpuSystem    uint64       `json:"-"`
	CpuContainer uint64       `json:"-"`
	PrevNet      prevNetStats `json:"-"`
	PrevReadTime time.Time    `json:"-"`
}

// Labels declaring the container under the control of an idle-sleep manager.
// sablier.enable is Sablier's convention (sablier.dev); closeyourit.idle_managed is ours,
// for whoever uses another manager or wants to mark a container by hand.
var idleManagedLabels = [...]string{"sablier.enable", "closeyourit.idle_managed"}

// IsIdleManaged reports whether the labels declare the container managed by an idle-sleep manager,
// which may therefore stop it on purpose. Absent or with any other value = not managed: when in
// doubt a disappearing container remains a failure to report.
func IsIdleManaged(labels map[string]string) bool {
	for _, key := range idleManagedLabels {
		value, ok := labels[key]
		if ok && (strings.EqualFold(value, "true") || value == "1") {
			return true
		}
	}
	return false
}
