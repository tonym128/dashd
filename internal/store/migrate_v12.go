package store

import "database/sql"

// The version 12 step: price history, budgets, plans, commits and rate limits.
var v12Planning = migration{
	version: 12,
	name:    "price history, budgets, plans, commits and rate limits",
	apply: func(tx *sql.Tx) error {
		for _, q := range []string{
			// One row per change of a model's price, from valid_from (epoch
			// seconds, 0 for "always") until the next row.
			`CREATE TABLE IF NOT EXISTS price_history (
	model      TEXT    NOT NULL,
	valid_from INTEGER NOT NULL DEFAULT 0,
	rates      TEXT    NOT NULL,
	source     TEXT    NOT NULL DEFAULT 'catalogue',
	PRIMARY KEY (model, valid_from)
) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS budget (
	scope   TEXT NOT NULL,
	name    TEXT NOT NULL,
	monthly REAL NOT NULL,
	PRIMARY KEY (scope, name)
) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS plan (
	name    TEXT PRIMARY KEY,
	agent   TEXT NOT NULL,
	monthly REAL NOT NULL
) WITHOUT ROWID`,
			`CREATE TABLE IF NOT EXISTS commit_log (
	project TEXT    NOT NULL,
	hash    TEXT    NOT NULL,
	ts      INTEGER NOT NULL,
	subject TEXT    NOT NULL DEFAULT '',
	machine TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (project, hash)
) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS commit_log_ts ON commit_log(project, ts)`,
			`CREATE TABLE IF NOT EXISTS rate_limit (
	session_uid TEXT    NOT NULL,
	ts          INTEGER NOT NULL,
	agent       TEXT    NOT NULL,
	machine     TEXT    NOT NULL DEFAULT '',
	detail      TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (session_uid, ts)
) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS rate_limit_ts ON rate_limit(ts)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	},
}
