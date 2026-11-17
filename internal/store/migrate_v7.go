package store

import "database/sql"

// The version 7 step: track connected push agents (last seen timestamp and
// cumulative pushed counts) so the System tab can report machine health.
var v7MachineStatus = migration{
	version: 7,
	name:    "track remote push machine heartbeats",
	apply: func(tx *sql.Tx) error {
		return execScript(`
CREATE TABLE IF NOT EXISTS machine_status (
	machine TEXT PRIMARY KEY,
	last_seen_at INTEGER NOT NULL DEFAULT 0,
	sessions_pushed INTEGER NOT NULL DEFAULT 0,
	calls_pushed INTEGER NOT NULL DEFAULT 0
);
`)(tx)
	},
}
