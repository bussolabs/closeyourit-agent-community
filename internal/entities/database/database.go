// Package database is the wire type of the `database` block of the agent payload: the state of a
// database detected on the host (PostgreSQL only for now). Readable keys (snake_case), like
// `host`; the block is optional top-level in the Payload and is omitted on hosts without a database.
package database

// Database is the snapshot of the host database state sent to CloseYourIt.
//
// Reachable=false when the database is detected but NOT probeable (down or auth denied): the
// sub-objects may be missing, but the block is sent anyway so the server can raise the "db down" alert.
type Database struct {
	Engine    string `json:"engine"`
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	Role      string `json:"role,omitempty"` // "primary" | "standby"
	// SystemIdentifier identifies the CLUSTER (pg_control_system()): it is identical on the primary
	// and on all its physical replicas, so it certifies which hosts belong together without having
	// to infer it from the database lists. String and not number: it is a 64-bit integer, and in JSON
	// such a large value loses precision in parsers that read numbers as floats.
	SystemIdentifier string         `json:"system_identifier,omitempty"`
	Connections      *Connections   `json:"connections,omitempty"`
	Replication      *Replication   `json:"replication,omitempty"`
	Databases        []DatabaseSize `json:"databases,omitempty"`
	TopTables        []TableSize    `json:"top_tables,omitempty"`
}

// Connections summarizes pg_stat_activity: total connections, ceiling (max_connections) and
// breakdown by state (normalized keys, e.g. "idle_in_transaction").
type Connections struct {
	Total    int            `json:"total"`
	Max      int            `json:"max,omitempty"`
	Reserved int            `json:"reserved,omitempty"`
	ByState  map[string]int `json:"by_state,omitempty"`
}

// Replication describes role and lag: on a primary the list of connected replicas, on a standby the
// replay lag. LagSeconds is a pointer because "absent" (nil) differs from "0" (in sync).
type Replication struct {
	Streaming  bool      `json:"streaming"`
	LagSeconds *float64  `json:"lag_seconds,omitempty"`
	Replicas   []Replica `json:"replicas,omitempty"`
}

// Replica is a standby connected to a primary (from pg_stat_replication).
type Replica struct {
	Client string `json:"client,omitempty"`
	State  string `json:"state,omitempty"`
}

// DatabaseSize is the on-disk size of a database (pg_database_size).
type DatabaseSize struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// TableSize is the total size of a table (pg_total_relation_size), with the database it belongs to.
type TableSize struct {
	Database  string `json:"database"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}
