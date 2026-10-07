// Package agent collects system metrics (CPU, memory, disks, network, sensors, GPU,
// Docker/Podman containers, systemd services, SMART).
//
// The collection is DERIVED from the Beszel agent (github.com/henrygd/beszel, MIT — see NOTICE.md);
// the transport is rewritten: no WebSocket/SSH/CBOR, the data is pushed via HTTP JSON to
// CloseYourIt by the pusher package.
package agent

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bussolabs/closeyourit-agent/internal/agent/deltatracker"
	"github.com/bussolabs/closeyourit-agent/internal/agent/utils"
	"github.com/bussolabs/closeyourit-agent/internal/common"
	"github.com/bussolabs/closeyourit-agent/internal/entities/journal"
	"github.com/bussolabs/closeyourit-agent/internal/entities/smart"
	"github.com/bussolabs/closeyourit-agent/internal/entities/system"
	beszel "github.com/bussolabs/closeyourit-agent/internal/version"
)

const defaultDataCacheTimeMs uint16 = 60_000

type Agent struct {
	sync.Mutex                                                                      // Used to lock agent while collecting data
	debug                     bool                                                  // true if LOG_LEVEL is set to debug
	zfs                       bool                                                  // true if system has arcstats
	memCalc                   string                                                // Memory calculation formula
	fsNames                   []string                                              // List of filesystem device names being monitored
	fsStats                   map[string]*system.FsStats                            // Keeps track of disk stats for each filesystem
	diskPrev                  map[uint16]map[string]prevDisk                        // Previous disk I/O counters per cache interval
	diskUsageCacheDuration    time.Duration                                         // How long to cache disk usage (to avoid waking sleeping disks)
	lastDiskUsageUpdate       time.Time                                             // Last time disk usage was collected
	netInterfaces             map[string]struct{}                                   // Stores all valid network interfaces
	netIoStats                map[uint16]system.NetIoStats                          // Keeps track of bandwidth usage per cache interval
	netInterfaceDeltaTrackers map[uint16]*deltatracker.DeltaTracker[string, uint64] // Per-cache-time NIC delta trackers
	dockerManager             *dockerManager                                        // Manages Docker API requests
	sensorConfig              *SensorConfig                                         // Sensors config
	systemInfo                system.Info                                           // Host system info (dynamic)
	systemDetails             system.Details                                        // Host system details (static)
	detailsDirty              bool                                                  // Whether system details have changed
	gpuManager                *GPUManager                                           // Manages GPU data
	cache                     *systemDataCache                                      // Cache for system stats based on cache time
	dataDir                   string                                                // Directory for persisting data
	smartManager              *SmartManager                                         // Manages SMART data
	systemdManager            *systemdManager                                       // Manages systemd services
	journalManager            *journalManager                                       // Collects native journald logs
	processManager            *processManager                                       // Collects top-N processes by cpu/memory
}

// NewAgent creates a new agent with the given data directory for persisting data.
// If the data directory is not set, it will attempt to find the optimal directory.
func NewAgent(dataDir ...string) (agent *Agent, err error) {
	agent = &Agent{
		fsStats: make(map[string]*system.FsStats),
		cache:   NewSystemDataCache(),
	}

	// Initialize disk I/O previous counters storage
	agent.diskPrev = make(map[uint16]map[string]prevDisk)
	// Initialize per-cache-time network tracking structures
	agent.netIoStats = make(map[uint16]system.NetIoStats)
	agent.netInterfaceDeltaTrackers = make(map[uint16]*deltatracker.DeltaTracker[string, uint64])

	agent.dataDir, err = GetDataDir(dataDir...)
	if err != nil {
		slog.Warn("Data directory not found")
	} else {
		slog.Info("Data directory", "path", agent.dataDir)
	}

	agent.memCalc, _ = utils.GetEnv("MEM_CALC")
	agent.sensorConfig = agent.newSensorConfig()

	// Parse disk usage cache duration (e.g., "15m", "1h") to avoid waking sleeping disks
	if diskUsageCache, exists := utils.GetEnv("DISK_USAGE_CACHE"); exists {
		if duration, err := time.ParseDuration(diskUsageCache); err == nil {
			agent.diskUsageCacheDuration = duration
			slog.Info("DISK_USAGE_CACHE", "duration", duration)
		} else {
			slog.Warn("Invalid DISK_USAGE_CACHE", "err", err)
		}
	}

	// Set up slog with a log level determined by the LOG_LEVEL env var
	if logLevelStr, exists := utils.GetEnv("LOG_LEVEL"); exists {
		switch strings.ToLower(logLevelStr) {
		case "debug":
			agent.debug = true
			slog.SetLogLoggerLevel(slog.LevelDebug)
		case "warn":
			slog.SetLogLoggerLevel(slog.LevelWarn)
		case "error":
			slog.SetLogLoggerLevel(slog.LevelError)
		}
	}

	slog.Debug(beszel.Version)

	// initialize docker manager
	agent.dockerManager = newDockerManager(agent)

	// initialize system info
	agent.refreshSystemDetails()

	// SMART_INTERVAL env var to update smart data at this interval
	if smartIntervalEnv, exists := utils.GetEnv("SMART_INTERVAL"); exists {
		if duration, err := time.ParseDuration(smartIntervalEnv); err == nil && duration > 0 {
			agent.systemDetails.SmartInterval = duration
			slog.Info("SMART_INTERVAL", "duration", duration)
		} else {
			slog.Warn("Invalid SMART_INTERVAL", "err", err)
		}
	}

	// initialize disk info
	agent.initializeDiskInfo()

	// initialize net io stats
	agent.initializeNetIoStats()

	agent.systemdManager, err = newSystemdManager()
	if err != nil {
		slog.Debug("Systemd", "err", err)
	}

	agent.smartManager, err = NewSmartManager()
	if err != nil {
		slog.Debug("SMART", "err", err)
	}
	if agent.smartManager != nil && agent.systemDetails.SmartInterval > 0 {
		agent.smartManager.collectInterval = agent.systemDetails.SmartInterval
	}

	// initialize journal manager (native journald logs; nil if journalctl is missing or SKIP_JOURNAL)
	agent.journalManager = newJournalManager(agent.dataDir, agent.systemdManager)

	// initialize process manager (top-N by cpu/memory; SKIP_PROCESSES to disable)
	if skip, _ := utils.GetEnv("SKIP_PROCESSES"); skip != "true" {
		agent.processManager = newProcessManager()
	}

	// initialize GPU manager
	agent.gpuManager, err = NewGPUManager()
	if err != nil {
		slog.Debug("GPU", "err", err)
	}

	// if debugging, print stats
	if agent.debug {
		slog.Debug("Stats", "data", agent.gatherStats(common.DataRequestOptions{CacheTimeMs: defaultDataCacheTimeMs, IncludeDetails: true}))
	}

	return agent, nil
}

// Collect gathers (or serves from the cache) the system data for the given interval. It is the
// public entry point used by the push loop; IncludeDetails is always true (the payload always
// carries `host`).
func (a *Agent) Collect(cacheTimeMs uint16) *system.CombinedData {
	if cacheTimeMs == 0 {
		cacheTimeMs = defaultDataCacheTimeMs
	}
	return a.gatherStats(common.DataRequestOptions{CacheTimeMs: cacheTimeMs, IncludeDetails: true})
}

// Fingerprint stably identifies the machine (sha256(machine-id||hostname+cpu)[:24], derived from
// Beszel): it is the host self-registration key on CloseYourIt.
func (a *Agent) Fingerprint() string {
	return GetFingerprint(a.dataDir, a.systemDetails.Hostname, a.systemDetails.CpuModel)
}

// Details exposes the static machine data (hostname, kernel, cores, cpu, os, memory).
func (a *Agent) Details() system.Details {
	return a.systemDetails
}

// SmartData refreshes (rate-limited by SMART_INTERVAL inside the manager) and returns the current
// SMART result per device. Nil-safe: without smartctl it returns an empty map.
func (a *Agent) SmartData() map[string]smart.SmartData {
	if a.smartManager == nil {
		return map[string]smart.SmartData{}
	}
	if err := a.smartManager.Refresh(false); err != nil {
		slog.Debug("SMART refresh", "err", err)
	}
	return a.smartManager.GetCurrentData()
}

// JournalReport collects native journald logs (err/crit stream + snapshots of failed units).
// Nil-safe: without journalctl (or SKIP_JOURNAL, or non-linux) it returns nil → `journal` block
// omitted. The cursor advances in memory; CommitJournal persists it only after a successful push.
func (a *Agent) JournalReport() *journal.Report {
	if a.journalManager == nil {
		return nil
	}
	return a.journalManager.Collect()
}

// CommitJournal persists the journald cursor after a successful push (see main). It must NOT be
// called on a failed push: the stream is resent and the server deduplicates on [host, cursor].
func (a *Agent) CommitJournal() {
	if a.journalManager != nil {
		a.journalManager.Commit()
	}
}

func (a *Agent) gatherStats(options common.DataRequestOptions) *system.CombinedData {
	a.Lock()
	defer a.Unlock()

	cacheTimeMs := options.CacheTimeMs
	data, isCached := a.cache.Get(cacheTimeMs)
	if isCached {
		slog.Debug("Cached data", "cacheTimeMs", cacheTimeMs)
		return data
	}

	*data = system.CombinedData{
		Stats: a.getSystemStats(cacheTimeMs),
		Info:  a.systemInfo,
	}

	if a.dockerManager != nil {
		if containerStats, err := a.dockerManager.getDockerStats(cacheTimeMs); err == nil {
			data.Containers = containerStats
			slog.Debug("Containers", "data", data.Containers)
		} else {
			slog.Debug("Containers", "err", err)
		}
	}

	// The systemd manager refreshes itself on its own 10-minute worker, so the
	// payload can always carry the latest stats. Gating this on the exact 60s
	// cache interval (a beszel leftover: its hub polled at several cadences)
	// silently dropped services and the failed counter for any other CYI_INTERVAL.
	if a.systemdManager != nil {
		totalCount := uint16(a.systemdManager.getServiceStatsCount())
		if totalCount > 0 {
			numFailed := a.systemdManager.getFailedServiceCount()
			data.Info.Services = []uint16{totalCount, numFailed}
		}
		if a.systemdManager.hasFreshStats {
			data.SystemdServices = a.systemdManager.getServiceStats(nil, false)
		}
	}

	if a.processManager != nil {
		data.Stats.TopProcesses = a.processManager.getStats()
	}

	data.Stats.ExtraFs = make(map[string]*system.FsStats)
	data.Info.ExtraFsPct = make(map[string]float64)
	for name, stats := range a.fsStats {
		if !stats.Root && stats.DiskTotal > 0 {
			// Use custom name if available, otherwise use device name
			key := name
			if stats.Name != "" {
				key = stats.Name
			}
			data.Stats.ExtraFs[key] = stats
			// Add percentages to Info struct for dashboard
			if stats.DiskTotal > 0 {
				pct := utils.TwoDecimals((stats.DiskUsed / stats.DiskTotal) * 100)
				data.Info.ExtraFsPct[key] = pct
			}
		}
	}
	slog.Debug("Extra FS", "data", data.Stats.ExtraFs)

	a.cache.Set(data, cacheTimeMs)

	return a.attachSystemDetails(data, cacheTimeMs, options.IncludeDetails)
}
