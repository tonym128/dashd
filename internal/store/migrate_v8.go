package store

import "database/sql"

// The version 8 step: record when the store ingested each session and call
// (Unix seconds) so central servers can distinguish event time from arrival time.
var v8IngestedAt = migration{
	version: 8,
	name:    "record ingested_at timestamp on session and call",
	apply: func(tx *sql.Tx) error {
		for _, table := range []string{"session", "call"} {
			if err := addColumn(tx, table, "ingested_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
				return err
			}
		}
		return nil
	},
}
