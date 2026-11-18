package store

import "database/sql"

// The version 9 step:
//
//   - a key/value meta table, which holds the server's instance id (so push
//     clients notice a rebuilt server and resend history), the prune floor
//     (so a pruned window is not re-ingested from a log that still exists),
//     and push-client bookkeeping in push-state databases;
//   - last-error and version columns on machine_status, so a machine whose
//     pushes are failing is distinguishable from one that is switched off.
var v9MetaAndMachineHealth = migration{
	version: 9,
	name:    "meta table and machine health columns",
	apply: func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID`); err != nil {
			return err
		}
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS machine_status (
	machine TEXT PRIMARY KEY,
	last_seen_at INTEGER NOT NULL DEFAULT 0,
	sessions_pushed INTEGER NOT NULL DEFAULT 0,
	calls_pushed INTEGER NOT NULL DEFAULT 0
)`); err != nil {
			return err
		}
		for _, c := range []struct{ col, decl string }{
			{"last_error", "TEXT NOT NULL DEFAULT ''"},
			{"last_error_at", "INTEGER NOT NULL DEFAULT 0"},
			{"version", "TEXT NOT NULL DEFAULT ''"},
		} {
			if err := addColumn(tx, "machine_status", c.col, c.decl); err != nil {
				return err
			}
		}
		return nil
	},
}
