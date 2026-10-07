package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner picks the answer based on a keyword contained in the query (keywords are mutually
// exclusive for each probe query). The value "ERR" simulates an error.
func fakeRunner(table map[string]string) psqlRunner {
	return func(query string) (string, error) {
		for key, out := range table {
			if strings.Contains(query, key) {
				if out == "ERR" {
					return "", fmt.Errorf("simulated error for %q", key)
				}
				return out, nil
			}
		}
		return "", fmt.Errorf("query not handled in the test: %s", query)
	}
}

func line(fields ...string) string { return strings.Join(fields, psqlSep) }

func TestProbePostgres_UnreachableDatabase(t *testing.T) {
	db := probePostgres(fakeRunner(map[string]string{"SELECT 1": "ERR"}))

	require.NotNil(t, db)
	assert.Equal(t, "postgresql", db.Engine)
	assert.False(t, db.Reachable, "a failed SELECT 1 must mark the database unreachable")
	assert.Empty(t, db.Role)
	assert.Nil(t, db.Connections)
}

func TestProbePostgres_PrimaryHappyPath(t *testing.T) {
	db := probePostgres(fakeRunner(map[string]string{
		"SELECT 1":                       "1",
		"server_version":                 "16.2",
		"pg_is_in_recovery":              "f",
		"pg_control_system":              "7234567890123456789",
		"pg_stat_activity":               line("active", "5") + "\n" + line("idle", "30") + "\n" + line("idle in transaction", "7"),
		"max_connections":                "100",
		"superuser_reserved_connections": "5",
		"pg_stat_replication":            line("192.0.2.10", "streaming", "0.12"),
		"pg_database":                    line("app_production", "123456789") + "\n" + line("postgres", "8000000"),
		"pg_stat_user_tables":            line("public.events", "98765432"),
	}))

	require.NotNil(t, db)
	assert.True(t, db.Reachable)
	assert.Equal(t, "16.2", db.Version)
	assert.Equal(t, "primary", db.Role)
	assert.Equal(t, "7234567890123456789", db.SystemIdentifier)

	require.NotNil(t, db.Connections)
	assert.Equal(t, 42, db.Connections.Total)
	assert.Equal(t, 100, db.Connections.Max)
	assert.Equal(t, 5, db.Connections.Reserved)
	assert.Equal(t, 5, db.Connections.ByState["active"])
	assert.Equal(t, 30, db.Connections.ByState["idle"])
	assert.Equal(t, 7, db.Connections.ByState["idle_in_transaction"], "spaces must be normalized to underscores")

	require.NotNil(t, db.Replication)
	assert.True(t, db.Replication.Streaming)
	require.NotNil(t, db.Replication.LagSeconds)
	assert.InDelta(t, 0.12, *db.Replication.LagSeconds, 0.0001)
	require.Len(t, db.Replication.Replicas, 1)
	assert.Equal(t, "192.0.2.10", db.Replication.Replicas[0].Client)

	require.Len(t, db.Databases, 2)
	assert.Equal(t, "app_production", db.Databases[0].Name)
	assert.Equal(t, int64(123456789), db.Databases[0].SizeBytes)

	require.Len(t, db.TopTables, 1)
	assert.Equal(t, "public.events", db.TopTables[0].Name)
	assert.Equal(t, int64(98765432), db.TopTables[0].SizeBytes)
}

// The point of the field: a primary and its replica report the SAME cluster identifier. It is what
// lets the server recognize the copies with certainty instead of inferring them from database lists.
func TestProbePostgres_StandbyReportsTheSameSystemIdentifier(t *testing.T) {
	const identifier = "7234567890123456789"

	primary := probePostgres(fakeRunner(map[string]string{
		"SELECT 1":            "1",
		"pg_is_in_recovery":   "f",
		"pg_control_system":   identifier,
		"server_version":      "16.2",
		"pg_stat_activity":    line("active", "1"),
		"max_connections":     "100",
		"pg_stat_replication": line("192.0.2.11", "streaming", "0"),
		"pg_database":         line("app_production", "10"),
		"pg_stat_user_tables": "",
	}))
	standby := probePostgres(fakeRunner(map[string]string{
		"SELECT 1":               "1",
		"pg_is_in_recovery":      "t",
		"pg_control_system":      identifier,
		"server_version":         "16.2",
		"pg_stat_activity":       line("active", "1"),
		"max_connections":        "100",
		"pg_last_wal_replay_lsn": "0.1",
		"pg_stat_wal_receiver":   "t",
		"pg_database":            line("app_production", "10"),
		"pg_stat_user_tables":    "",
	}))

	assert.Equal(t, "primary", primary.Role)
	assert.Equal(t, "standby", standby.Role)
	assert.Equal(t, identifier, primary.SystemIdentifier)
	assert.Equal(t, identifier, standby.SystemIdentifier,
		"the system_identifier belongs to the CLUSTER: identical on primary and replicas, even in recovery")
}

// The permission on pg_control_system() may be missing (it is reserved to superusers by default):
// the field stays empty and the rest of the block is sent anyway — the server degrades to the
// heuristic on its own.
func TestProbePostgres_SystemIdentifierUnreadable(t *testing.T) {
	db := probePostgres(fakeRunner(map[string]string{
		"SELECT 1":            "1",
		"pg_is_in_recovery":   "f",
		"pg_control_system":   "ERR",
		"server_version":      "16.2",
		"pg_stat_activity":    line("active", "1"),
		"max_connections":     "100",
		"pg_stat_replication": "",
		"pg_database":         line("app_production", "10"),
		"pg_stat_user_tables": "",
	}))

	require.NotNil(t, db)
	assert.Empty(t, db.SystemIdentifier)
	assert.True(t, db.Reachable, "a failed optional probe does not make the database unreachable")
	assert.Equal(t, "16.2", db.Version)
	assert.Equal(t, "primary", db.Role)
	require.Len(t, db.Databases, 1)
}

func TestProbeReplication_Standby(t *testing.T) {
	run := fakeRunner(map[string]string{
		"pg_last_wal_replay_lsn": "3.5",
		"pg_stat_wal_receiver":   "t",
	})

	r := probeReplication(run, true)

	require.NotNil(t, r)
	assert.True(t, r.Streaming)
	require.NotNil(t, r.LagSeconds)
	assert.InDelta(t, 3.5, *r.LagSeconds, 0.0001)
	assert.Empty(t, r.Replicas, "a standby lists no downstream replicas")
}

func TestProbeReplication_InSyncStandbyReportsZeroLag(t *testing.T) {
	run := fakeRunner(map[string]string{
		"pg_last_wal_replay_lsn": "0",
		"pg_stat_wal_receiver":   "t",
	})

	r := probeReplication(run, true)

	assert.True(t, r.Streaming)
	require.NotNil(t, r.LagSeconds)
	assert.Zero(t, *r.LagSeconds, "receive LSN equal to replay LSN means no backlog")
}

func TestProbeReplication_DisconnectedStandbyIsNotStreaming(t *testing.T) {
	run := fakeRunner(map[string]string{
		"pg_last_wal_replay_lsn": "12.5",
		"pg_stat_wal_receiver":   "",
	})

	r := probeReplication(run, true)

	assert.False(t, r.Streaming, "an old receive LSN does not prove the WAL receiver is active")
	require.NotNil(t, r.LagSeconds)
	assert.InDelta(t, 12.5, *r.LagSeconds, 0.0001)
}

func TestProbeReplication_PrimaryWithoutReplicas(t *testing.T) {
	// empty pg_stat_replication → no replicas, streaming false, lag nil.
	r := probeReplication(fakeRunner(map[string]string{"pg_stat_replication": ""}), false)

	require.NotNil(t, r)
	assert.False(t, r.Streaming)
	assert.Nil(t, r.LagSeconds)
	assert.Empty(t, r.Replicas)
}

func TestProbeConnections_TotalAndStates(t *testing.T) {
	run := fakeRunner(map[string]string{
		"pg_stat_activity":               line("active", "2") + "\n" + line("", "3"),
		"max_connections":                "200",
		"superuser_reserved_connections": "7",
	})

	c := probeConnections(run)

	require.NotNil(t, c)
	assert.Equal(t, 5, c.Total)
	assert.Equal(t, 200, c.Max)
	assert.Equal(t, 7, c.Reserved)
	assert.Equal(t, 2, c.ByState["active"])
	assert.Equal(t, 3, c.ByState["unknown"], "the empty state becomes 'unknown'")
}

func TestNormalizeState(t *testing.T) {
	assert.Equal(t, "idle_in_transaction", normalizeState("idle in transaction"))
	assert.Equal(t, "active", normalizeState("active"))
	assert.Equal(t, "unknown", normalizeState(""))
}
