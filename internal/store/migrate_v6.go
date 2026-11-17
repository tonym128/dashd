package store

import "database/sql"

// The version 6 step: record which machine each session, call, and tool call
// originated from, so a central dashd server can aggregate logs pushed from
// multiple local machines and filter or break down by host.
var v6Machine = migration{
	version: 6,
	name:    "record originating machine on session, call, and tool_call",
	apply: func(tx *sql.Tx) error {
		for _, table := range []string{"session", "call", "tool_call"} {
			if err := addColumn(tx, table, "machine", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
		return execScript(`
CREATE INDEX IF NOT EXISTS idx_call_machine ON call(machine);
CREATE INDEX IF NOT EXISTS idx_session_machine ON session(machine);
`)(tx)
	},
}
