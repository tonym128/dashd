package store

import "database/sql"

// The version 10 step: re-read Antigravity conversations whose calls were
// stored under a placeholder model name ("antigravity-model-1526") because
// the build that scanned them did not know the model id. Antigravity's cursor
// is a row id, so a conversation is never re-read on its own; dropping its
// cursor makes the next pass read it from the start as a replacement, under
// the names (and prices) this build knows. A conversation whose database is
// gone keeps its stored rows untouched.
var v10RereadUnknownAntigravity = migration{
	version: 10,
	name:    "re-read Antigravity conversations stored with unknown model ids",
	apply: func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			DELETE FROM scan_state
			WHERE agent = 'agy' AND session_uid IN (
				SELECT DISTINCT session_uid FROM call
				WHERE agent = 'agy' AND model LIKE 'antigravity-model-%')`)
		return err
	},
}
