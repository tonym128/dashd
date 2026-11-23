package store

import "database/sql"

// The version 11 step: re-read every Antigravity conversation, because the
// model id table earlier builds used named several models wrongly and the
// stored calls keep only the name. Unlike version 10 this does not look at the
// call table: a push agent's state database holds cursors and no calls.
// A conversation whose database is gone keeps its stored rows untouched.
var v11RereadAntigravity = migration{
	version: 11,
	name:    "re-read Antigravity conversations named by a wrong model id table",
	apply: func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM scan_state WHERE agent = 'agy'`)
		return err
	},
}
