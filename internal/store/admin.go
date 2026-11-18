package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- meta

// Meta returns a value from the meta table, or "" when it is unset.
func (s *Store) Meta(key string) (string, error) {
	var v string
	err := s.q.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta writes a value to the meta table.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.q.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// InstanceID identifies this database. It is created on first use and never
// changes, so a push client that sees a different id knows the server's
// database was replaced and that its own cursors no longer describe what the
// server holds.
func (s *Store) InstanceID() (string, error) {
	id, err := s.Meta("instance_id")
	if err != nil || id != "" {
		return id, err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id = hex.EncodeToString(b[:])
	_, err = s.q.Exec(`INSERT INTO meta (key, value) VALUES ('instance_id', ?) ON CONFLICT(key) DO NOTHING`, id)
	if err != nil {
		return "", err
	}
	return s.Meta("instance_id")
}

// PruneFloor is the cutoff of the most recent prune (Unix seconds), or 0.
// Calls older than it are not re-ingested.
func (s *Store) PruneFloor() (int64, error) {
	v, err := s.Meta("prune_floor")
	if err != nil || v == "" {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// ---------------------------------------------------------------- pruning

// PruneOptions controls PruneBefore.
type PruneOptions struct {
	// IncludeUndated also deletes calls with no timestamp (ts = 0), which no
	// date cutoff can otherwise reach.
	IncludeUndated bool
	// Vacuum reclaims disk space afterwards. It needs an exclusive lock, so a
	// busy server may make it fail; the prune itself is already committed.
	Vacuum bool
}

// PruneBefore deletes call and tool_call rows older than before, recomputes
// the summaries of every session that lost rows, removes sessions left with no
// calls, and records the cutoff as the prune floor so a log that still exists
// cannot bring the deleted window back.
func (s *Store) PruneBefore(ctx context.Context, before time.Time, opt PruneOptions) (int64, error) {
	cutoff := before.Unix()
	cond := `ts > 0 AND ts < ?`
	if opt.IncludeUndated {
		cond = `ts < ?`
	}
	var deleted int64
	var touched []string
	err := s.InTx(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT session_uid FROM call WHERE `+cond, cutoff)
		if err != nil {
			return err
		}
		for rows.Next() {
			var uid string
			if err := rows.Scan(&uid); err != nil {
				rows.Close()
				return err
			}
			touched = append(touched, uid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM call WHERE `+cond, cutoff)
		if err != nil {
			return fmt.Errorf("prune calls: %w", err)
		}
		deleted, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `DELETE FROM tool_call WHERE `+cond, cutoff); err != nil {
			return fmt.Errorf("prune tool_calls: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM session WHERE NOT EXISTS (SELECT 1 FROM call c WHERE c.session_uid = session.uid)`); err != nil {
			return fmt.Errorf("prune empty sessions: %w", err)
		}
		prev := int64(0)
		var v string
		if err := tx.QueryRow(`SELECT value FROM meta WHERE key = 'prune_floor'`).Scan(&v); err == nil {
			prev, _ = strconv.ParseInt(v, 10, 64)
		}
		if cutoff > prev {
			if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('prune_floor', ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(cutoff, 10)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	// Sessions that kept some calls still carry totals that include the
	// deleted ones until they are recomputed.
	for _, uid := range touched {
		if err := s.RecomputeSession(uid); err != nil {
			return deleted, fmt.Errorf("recompute %s after prune: %w", uid, err)
		}
	}
	if opt.Vacuum {
		if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
			return deleted, fmt.Errorf("vacuum after prune (rows are already deleted): %w", err)
		}
	}
	return deleted, nil
}

// ---------------------------------------------------------------- backups

// Backup writes a consistent copy of the database to dest using VACUUM INTO,
// which is safe while the server and scanner are running.
func (s *Store) Backup(ctx context.Context, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("backup to %s: %w", dest, err)
	}
	_ = os.Chmod(dest, 0o600)
	return nil
}

// backupBeforeMigration keeps a copy of a database about to be migrated, since
// migrations are one-way and an older binary refuses a newer schema.
func (s *Store) backupBeforeMigration(version int) {
	if s.Path == "" || s.Path == ":memory:" {
		return
	}
	dest := fmt.Sprintf("%s.bak-v%d", s.Path, version)
	if _, err := os.Stat(dest); err == nil {
		return
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, dest); err != nil {
		Warn("could not back up the database before migrating it", "dest", dest, "error", err)
		return
	}
	_ = os.Chmod(dest, 0o600)
	Warn("backed up the database before a schema migration", "from_version", version, "backup", dest)
}

// ---------------------------------------------------------------- ingest identity

// ResolveIngestUID maps a pushed session uid to the uid it is stored under.
//
// Sessions are keyed by uid alone, and uids are only unique per agent per
// machine. The first machine to push a uid keeps it; any other machine pushing
// the same uid gets it namespaced as "<machine>::<uid>". The mapping is a
// function of what is already stored, so every later push from either machine
// lands on the same row, and one machine can never replace another's session.
func (s *Store) ResolveIngestUID(uid, machine string) (string, error) {
	if machine == "" {
		return uid, nil
	}
	var owner string
	err := s.q.QueryRow(`SELECT machine FROM session WHERE uid = ?`, uid).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return uid, nil
	case err != nil:
		return "", err
	case owner == "" || owner == machine:
		return uid, nil
	}
	return machine + "::" + uid, nil
}

// ---------------------------------------------------------------- scan state

// ResetScanStates forgets every cursor, so the next pass re-reads every log
// from the start. Used by `dashd push -resync` and when the remote server's
// instance id changes.
func (s *Store) ResetScanStates() error {
	_, err := s.q.Exec(`DELETE FROM scan_state`)
	return err
}

// CountScanStates returns how many source files have a stored cursor.
func (s *Store) CountScanStates() (int, error) {
	var n int
	err := s.q.QueryRow(`SELECT COUNT(*) FROM scan_state`).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------- machines

// Heartbeat is what a push client reports each time it checks in.
type Heartbeat struct {
	Machine        string
	SessionsPushed int
	CallsPushed    int
	At             time.Time
	// Error is the client's most recent failure, "" when healthy.
	Error   string
	Version string
}

// RecordMachineHeartbeat records when a push client last checked in, how many
// sessions/calls it delivered, and whether it is currently failing.
func (s *Store) RecordMachineHeartbeat(h Heartbeat) error {
	if h.Machine == "" {
		return nil
	}
	errAt := int64(0)
	if h.Error != "" {
		errAt = h.At.Unix()
	}
	if len(h.Error) > 500 {
		h.Error = h.Error[:500]
	}
	_, err := s.q.Exec(`
		INSERT INTO machine_status (machine, last_seen_at, sessions_pushed, calls_pushed, last_error, last_error_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(machine) DO UPDATE SET
			last_seen_at = excluded.last_seen_at,
			sessions_pushed = machine_status.sessions_pushed + excluded.sessions_pushed,
			calls_pushed = machine_status.calls_pushed + excluded.calls_pushed,
			last_error = excluded.last_error,
			last_error_at = CASE WHEN excluded.last_error = '' THEN machine_status.last_error_at ELSE excluded.last_error_at END,
			version = CASE WHEN excluded.version = '' THEN machine_status.version ELSE excluded.version END`,
		h.Machine, h.At.Unix(), h.SessionsPushed, h.CallsPushed, h.Error, errAt, h.Version)
	return err
}

// RenameMachine moves every row from one machine name to another, merging into
// the target if it already exists.
func (s *Store) RenameMachine(from, to string) (int64, error) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return 0, fmt.Errorf("rename needs two different, non-empty machine names")
	}
	var n int64
	err := s.InTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE call SET machine = ? WHERE machine = ?`, to, from)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		for _, q := range []string{
			`UPDATE tool_call SET machine = ? WHERE machine = ?`,
			`UPDATE session SET machine = ? WHERE machine = ?`,
		} {
			if _, err := tx.Exec(q, to, from); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO machine_status (machine, last_seen_at, sessions_pushed, calls_pushed, last_error, last_error_at, version)
			SELECT ?, last_seen_at, sessions_pushed, calls_pushed, last_error, last_error_at, version
			FROM machine_status WHERE machine = ?
			ON CONFLICT(machine) DO UPDATE SET
				last_seen_at = MAX(machine_status.last_seen_at, excluded.last_seen_at),
				sessions_pushed = machine_status.sessions_pushed + excluded.sessions_pushed,
				calls_pushed = machine_status.calls_pushed + excluded.calls_pushed`, to, from); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM machine_status WHERE machine = ?`, from)
		return err
	})
	return n, err
}

// SessionExists reports whether uid is stored.
func (s *Store) SessionExists(uid string) (bool, error) {
	var one int
	err := s.q.QueryRow(`SELECT 1 FROM session WHERE uid = ?`, uid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
