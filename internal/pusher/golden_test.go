package pusher

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/bussolabs/closeyourit-agent/internal/entities/container"
	"github.com/bussolabs/closeyourit-agent/internal/entities/database"
	"github.com/bussolabs/closeyourit-agent/internal/entities/journal"
	"github.com/bussolabs/closeyourit-agent/internal/entities/smart"
	"github.com/bussolabs/closeyourit-agent/internal/entities/system"
	"github.com/bussolabs/closeyourit-agent/internal/entities/systemd"
)

// The "complete" sample the agent can produce: every optional wire block is populated.
// It lives here because it is the same payload the golden file describes, and it is reused by the
// check against the vendored contract (contract_test.go) so as not to describe the same thing twice.
func sharedGoldenPayload() *Payload {
	lagSeconds := 0.12
	return &Payload{
		Fingerprint:  "a1b2c3d4e5f60718293a4b5c",
		AgentVersion: "0.1.0",
		RecordedAt:   time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
		Host: HostInfo{
			Hostname:                 "apps-staging",
			Kernel:                   "6.8.0-59-generic",
			Cores:                    4,
			Threads:                  8,
			CpuModel:                 "AMD EPYC 9454P 48-Core Processor",
			OsName:                   "Ubuntu 24.04.2 LTS",
			Arch:                     "amd64",
			MemoryTotal:              8_127_683_584,
			RebootRequired:           true,
			UpdatesAvailable:         12,
			SecurityUpdatesAvailable: 3,
		},
		Data: &system.CombinedData{
			Stats: system.Stats{
				Cpu:          12.34,
				Mem:          7.57,
				MemUsed:      3.02,
				MemPct:       39.9,
				MemBuffCache: 2.51,
				Swap:         4.0,
				SwapUsed:     0.25,
				DiskTotal:    75.35,
				DiskUsed:     41.2,
				DiskPct:      54.68,
				InodeTotal:   1_000_000,
				InodeUsed:    400_000,
				InodePct:     40,
				Temperatures: map[string]float64{"k10temp_tctl": 48.5, "nvme_composite": 41.0},
				ExtraFs: map[string]*system.FsStats{
					"sdb1": {Mountpoint: "/mnt/data", DiskTotal: 250.0, DiskUsed: 100.5, InodeTotal: 2_000_000, InodeUsed: 1_500_000, InodePct: 75, DiskReadPs: 0.0, DiskWritePs: 0.1, DiskReadBytes: 0, DiskWriteBytes: 4096},
				},
				LoadAvg:           [3]float64{0.42, 0.38, 0.3},
				NetworkInterfaces: map[string][4]uint64{"eth0": {123456, 654321, 987654321, 1234567890}},
				DiskIO:            [2]uint64{1048576, 2097152},
				CpuBreakdown:      []float64{8.1, 3.2, 0.5, 0.1, 88.1},
				CpuCoresUsage:     system.Uint8Slice{10, 15, 12, 11},
				Bandwidth:         [2]uint64{524288, 1048576},
			},
			Info: system.Info{
				Threads:        8,
				Uptime:         864000,
				Cpu:            12.34,
				MemPct:         39.9,
				DiskPct:        54.68,
				AgentVersion:   "0.1.0",
				GpuPct:         7.5,
				DashboardTemp:  48.5,
				BandwidthBytes: 1572864,
				LoadAvg:        [3]float64{0.42, 0.38, 0.3},
				Services:       []uint16{42, 1},
			},
			Containers: []*container.Stats{
				{Id: "abcdef123456", Name: "closeyourit-web", Cpu: 2.5, Mem: 512.25, Bandwidth: [2]uint64{10240, 20480},
					Health: container.DockerHealthHealthy, Status: "running", Image: "registry.example.com/closeyourit:latest", Running: true, RestartCount: 2, StartedAt: "2026-07-02T09:00:00Z"},
				{Id: "fedcba654321", Name: "kamal-proxy", Cpu: 0.3, Mem: 32.5, Bandwidth: [2]uint64{512, 1024},
					Health: container.DockerHealthNone, Status: "running", Image: "basecamp/kamal-proxy:v0.8.4", Running: true},
			},
			SystemdServices: []*systemd.Service{
				{Name: "ssh.service", State: systemd.StatusActive, Cpu: 0.1, Mem: 10485760, MemPeak: 20971520, Sub: systemd.SubStateRunning, CpuPeak: 0.5},
				{Name: "backup.service", State: systemd.StatusFailed, Cpu: 0.0, Mem: 0, MemPeak: 52428800, Sub: systemd.SubStateFailed, CpuPeak: 12.0},
			},
		},
		Smart: map[string]smart.SmartData{
			"nvme0": {
				ModelName:       "SAMSUNG MZVL2512HCJQ-00B00",
				SerialNumber:    "S64KNX2T216015",
				FirmwareVersion: "GXA7601Q",
				Capacity:        512110190592,
				SmartStatus:     "PASSED",
				DiskName:        "/dev/nvme0",
				DiskType:        "nvme",
				Temperature:     41,
			},
		},
		Journal: &journal.Report{
			Entries: []journal.Entry{
				{Cursor: "s=abc123;i=1a2b;b=boot0001;m=100200300;t=6551abc;x=deadbeef", Time: 1782986400000000, Priority: 3, Unit: "backup", Message: "Backup job failed: connection refused"},
				{Cursor: "s=abc123;i=1a2c;b=boot0001;m=100200400;t=6551abd;x=cafebabe", Time: 1782986401000000, Priority: 2, Unit: "kernel", Message: "Out of memory: Killed process 4321 (backup)"},
			},
			Snapshots: map[string]journal.Snapshot{
				"backup.service": {CapturedAt: 1782986400, Lines: []string{
					"2026-07-02T10:00:00+0000 apps-staging systemd[1]: Starting backup.service...",
					"2026-07-02T10:00:01+0000 apps-staging backup[4321]: connection refused",
					"2026-07-02T10:00:01+0000 apps-staging systemd[1]: backup.service: Main process exited, code=exited, status=1/FAILURE",
				}},
			},
		},
		Database: &database.Database{
			Engine:           "postgresql",
			Reachable:        true,
			Version:          "16.2",
			Role:             "primary",
			SystemIdentifier: "7234567890123456789",
			Connections: &database.Connections{
				Total:    42,
				Max:      100,
				Reserved: 5,
				ByState:  map[string]int{"active": 5, "idle": 30, "idle_in_transaction": 7},
			},
			Replication: &database.Replication{
				Streaming:  true,
				LagSeconds: &lagSeconds,
				Replicas:   []database.Replica{{Client: "192.0.2.10", State: "streaming"}},
			},
			Databases: []database.DatabaseSize{{Name: "app_production", SizeBytes: 123456789}},
			TopTables: []database.TableSize{{Database: "app_production", Name: "public.events", SizeBytes: 98765432}},
		},
	}
}

// The golden file testdata/agent_payload.json is the wire CONTRACT shared with closeyourit-rails
// (copied into spec/fixtures/servers/agent_payload.json): if this test breaks, the server-side
// Normalize must be updated in the same change.
func TestPayloadMatchesSharedGoldenFile(t *testing.T) {
	payload := sharedGoldenPayload()

	produced, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden, err := os.ReadFile("testdata/agent_payload.json")
	if err != nil {
		t.Fatalf("golden: %v", err)
	}

	var producedMap, goldenMap map[string]any
	if err := json.Unmarshal(produced, &producedMap); err != nil {
		t.Fatalf("unmarshal produced: %v", err)
	}
	if err := json.Unmarshal(golden, &goldenMap); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}

	if !reflect.DeepEqual(producedMap, goldenMap) {
		producedPretty, _ := json.MarshalIndent(producedMap, "", "  ")
		t.Fatalf("payload diverges from the shared golden.\nproduced:\n%s", producedPretty)
	}
}
