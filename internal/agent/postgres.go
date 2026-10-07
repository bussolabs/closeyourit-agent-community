package agent

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bussolabs/closeyourit-agent/internal/entities/database"
)

// psqlSep is the field separator passed to psql (-F): a character that never appears in PostgreSQL
// names nor in numbers, so splitting the lines is always correct.
const psqlSep = "\x1f"

// DatabaseReport detects a PostgreSQL database on the host and returns its state (status,
// connections, replication, sizes), probing it READ-ONLY via `psql`. Nil-safe: if no database is
// detected (and no override) it returns nil → `database` block omitted from the payload
// (backward compatible).
//
// Detection (autodiscovery): PostgreSQL unix socket, port 5432 listening, or active postgresql
// systemd unit. Override via CYI_AGENT_DB_ENABLE=true|false (default "auto").
// The probe runs with a timeout: a slow or down database does NOT compromise the rest of the push.
func DatabaseReport() *database.Database {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("CYI_AGENT_DB_ENABLE")))
	switch mode {
	case "false", "0", "off":
		return nil
	case "true", "1", "on":
		// probe forced even without detection
	default: // "auto" or empty
		if !postgresDetected() {
			return nil
		}
	}

	if _, err := exec.LookPath("psql"); err != nil {
		// database present but no client to probe it: do not make up a "down" state.
		slog.Warn("PostgreSQL detected but psql unavailable: probe skipped", "err", err)
		return nil
	}
	return probePostgres(runPsql)
}

// psqlRunner runs a query and returns its stdout (tuple-only, unaligned). Injectable in tests.
type psqlRunner func(query string) (string, error)

// postgresDetected does a best-effort detection WITHOUT connecting: default unix socket, port 5432
// listening on loopback, or active postgresql systemd unit.
func postgresDetected() bool {
	for _, pattern := range []string{"/var/run/postgresql/.s.PGSQL.*", "/run/postgresql/.s.PGSQL.*"} {
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return true
		}
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:5432", 300*time.Millisecond); err == nil {
		_ = conn.Close()
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "postgresql").Run(); err == nil {
		return true
	}
	return false
}

// runPsql launches `psql` read-only via the local socket, with a defensive timeout. It honors the
// standard libpq env vars (PGHOST/PGUSER/PGDATABASE) and the CYI_AGENT_DB_SOCKET/USER/DBNAME overrides.
func runPsql(query string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	args := []string{"-tAqX", "-F", psqlSep}
	if host := dbEnv("CYI_AGENT_DB_SOCKET", "PGHOST"); host != "" {
		args = append(args, "-h", host)
	}
	if user := dbEnv("CYI_AGENT_DB_USER", "PGUSER"); user != "" {
		args = append(args, "-U", user)
	}
	args = append(args, "-d", dbName(), "-c", query)

	cmd := exec.CommandContext(ctx, "psql", args...)
	// PGCONNECT_TIMEOUT keeps an unreachable DB from blocking the push loop.
	cmd.Env = append(os.Environ(), "PGCONNECT_TIMEOUT=3")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// dbName is the database to open the service connection on (default "postgres").
func dbName() string {
	if name := dbEnv("CYI_AGENT_DB_DBNAME", "PGDATABASE"); name != "" {
		return name
	}
	return "postgres"
}

// dbEnv returns the first non-empty env var among the given ones.
func dbEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// probePostgres runs the probe queries through run and assembles the snapshot. If the first
// SELECT 1 fails, the database is unreachable: it still returns the block with Reachable=false.
func probePostgres(run psqlRunner) *database.Database {
	db := &database.Database{Engine: "postgresql"}

	if _, err := run("SELECT 1"); err != nil {
		db.Reachable = false
		return db
	}
	db.Reachable = true

	if out, err := run("SHOW server_version"); err == nil {
		db.Version = strings.TrimSpace(out)
	}

	// Cluster identifier: identical on the primary and on all its physical replicas, so it certifies
	// which hosts belong together. It reads the control file, so it answers even in recovery: it is
	// asked of everyone, not only the primary. Optional — pg_control_system() is reserved to
	// superusers by default and the monitoring role may not have it: in that case the field stays
	// empty and the server falls back to inferring the copies as it did before.
	if out, err := run("SELECT system_identifier FROM pg_control_system()"); err == nil {
		db.SystemIdentifier = strings.TrimSpace(out)
	}

	inRecovery := false
	if out, err := run("SELECT pg_is_in_recovery()"); err == nil {
		inRecovery = strings.TrimSpace(out) == "t"
	}
	if inRecovery {
		db.Role = "standby"
	} else {
		db.Role = "primary"
	}

	db.Connections = probeConnections(run)
	db.Replication = probeReplication(run, inRecovery)
	db.Databases = probeDatabases(run)
	db.TopTables = probeTopTables(run)

	return db
}

func probeConnections(run psqlRunner) *database.Connections {
	out, err := run("SELECT COALESCE(state,'unknown'), count(*) FROM pg_stat_activity GROUP BY 1")
	if err != nil {
		return nil
	}
	c := &database.Connections{ByState: map[string]int{}}
	for _, line := range splitLines(out) {
		fields := strings.SplitN(line, psqlSep, 2)
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			continue
		}
		c.ByState[normalizeState(strings.TrimSpace(fields[0]))] += n
		c.Total += n
	}
	if maxOut, err := run("SHOW max_connections"); err == nil {
		if m, err := strconv.Atoi(strings.TrimSpace(maxOut)); err == nil {
			c.Max = m
		}
	}
	if reservedOut, err := run("SELECT current_setting('superuser_reserved_connections')::int + COALESCE(NULLIF(current_setting('reserved_connections', true), ''), '0')::int"); err == nil {
		if reserved, err := strconv.Atoi(strings.TrimSpace(reservedOut)); err == nil {
			c.Reserved = reserved
		}
	}
	return c
}

// normalizeState turns pg_stat_activity states into JSON-friendly keys ("idle in transaction"
// → "idle_in_transaction"); the empty state (background connections) becomes "unknown".
func normalizeState(s string) string {
	if s == "" {
		return "unknown"
	}
	return strings.ReplaceAll(s, " ", "_")
}

func probeReplication(run psqlRunner, inRecovery bool) *database.Replication {
	r := &database.Replication{}

	if inRecovery {
		// On an idle cluster the last transaction timestamp does not advance: if receive and replay
		// LSN match the standby is in sync and the real lag is zero, not the age of the last write.
		if out, err := run("SELECT CASE WHEN pg_last_xact_replay_timestamp() IS NULL THEN NULL WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0 ELSE GREATEST(EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp())), 0) END"); err == nil {
			if lag, ok := parseFloat(out); ok {
				r.LagSeconds = &lag
			}
		}
		if out, err := run("SELECT COALESCE(status = 'streaming', false) FROM pg_stat_wal_receiver LIMIT 1"); err == nil {
			r.Streaming = strings.TrimSpace(out) == "t"
		}
		return r
	}

	out, err := run("SELECT COALESCE(client_addr::text,''), COALESCE(state,''), COALESCE(EXTRACT(EPOCH FROM replay_lag),0) FROM pg_stat_replication")
	if err != nil {
		return r
	}
	var maxLag float64
	hasLag := false
	for _, line := range splitLines(out) {
		fields := strings.SplitN(line, psqlSep, 3)
		if len(fields) < 2 {
			continue
		}
		rep := database.Replica{Client: strings.TrimSpace(fields[0]), State: strings.TrimSpace(fields[1])}
		r.Replicas = append(r.Replicas, rep)
		if rep.State == "streaming" {
			r.Streaming = true
		}
		if len(fields) == 3 {
			if lag, ok := parseFloat(fields[2]); ok {
				hasLag = true
				if lag > maxLag {
					maxLag = lag
				}
			}
		}
	}
	if hasLag {
		r.LagSeconds = &maxLag
	}
	return r
}

func probeDatabases(run psqlRunner) []database.DatabaseSize {
	out, err := run("SELECT datname, pg_database_size(datname) FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY 2 DESC")
	if err != nil {
		return nil
	}
	var res []database.DatabaseSize
	for _, line := range splitLines(out) {
		fields := strings.SplitN(line, psqlSep, 2)
		if len(fields) != 2 {
			continue
		}
		size, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if err != nil {
			continue
		}
		res = append(res, database.DatabaseSize{Name: strings.TrimSpace(fields[0]), SizeBytes: size})
	}
	return res
}

// probeTopTables returns the largest tables of the connection database (pg_stat_user_tables).
// Capped via CYI_AGENT_DB_TOP_TABLES (default 10; 0 disables).
func probeTopTables(run psqlRunner) []database.TableSize {
	limit := 10
	if raw := strings.TrimSpace(os.Getenv("CYI_AGENT_DB_TOP_TABLES")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	if limit <= 0 {
		return nil
	}

	out, err := run("SELECT schemaname||'.'||relname, pg_total_relation_size(relid) FROM pg_stat_user_tables ORDER BY 2 DESC LIMIT " + strconv.Itoa(limit))
	if err != nil {
		return nil
	}
	name := dbName()
	var res []database.TableSize
	for _, line := range splitLines(out) {
		fields := strings.SplitN(line, psqlSep, 2)
		if len(fields) != 2 {
			continue
		}
		size, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if err != nil {
			continue
		}
		res = append(res, database.TableSize{Database: name, Name: strings.TrimSpace(fields[0]), SizeBytes: size})
	}
	return res
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func parseFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
