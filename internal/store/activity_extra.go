package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

// SaveCommits records commits, replacing ones already known.
func (s *Store) SaveCommits(machine string, commits []model.Commit) error {
	if len(commits) == 0 {
		return nil
	}
	return s.InTx(func(tx *sql.Tx) error {
		for _, c := range commits {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO commit_log (project, hash, ts, subject, machine) VALUES (?,?,?,?,?)`,
				c.Project, c.Hash, c.TS, c.Subject, machine); err != nil {
				return err
			}
		}
		return nil
	})
}

// CommitCost is a commit and what its project spent on the way to it.
type CommitCost struct {
	model.Commit
	Machine string
	Cost    float64
	Calls   int
}

// CommitCosts charges each commit with the spend in its project since the
// commit before it, or the preceding 24 hours for the first, newest first.
func (s *Store) CommitCosts(ctx context.Context, f Filter, limit int) ([]CommitCost, error) {
	q := `SELECT project, hash, ts, subject, machine FROM commit_log WHERE 1=1`
	var args []any
	if f.DateFrom != nil {
		// A day earlier, so the first commit in range knows its predecessor.
		q += ` AND ts >= ?`
		args = append(args, f.DateFrom.Add(-30*24*time.Hour).Unix())
	}
	if f.DateTo != nil {
		q += ` AND ts <= ?`
		args = append(args, f.DateTo.Unix())
	}
	if len(f.Projects) > 0 {
		q += ` AND project IN (` + inPlaceholders(len(f.Projects)) + `)`
		for _, p := range f.Projects {
			args = append(args, p)
		}
	}
	rows, err := s.q.QueryContext(ctx, q+` ORDER BY project, ts, hash`, args...)
	if err != nil {
		return nil, fmt.Errorf("commits: %w", err)
	}
	byProject := map[string][]CommitCost{}
	for rows.Next() {
		var c CommitCost
		if err := rows.Scan(&c.Project, &c.Hash, &c.TS, &c.Subject, &c.Machine); err != nil {
			rows.Close()
			return nil, err
		}
		byProject[c.Project] = append(byProject[c.Project], c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []CommitCost
	for project, commits := range byProject {
		cf := f
		cf.Projects = []string{project}
		// The window is per commit, not the page's dates.
		from := time.Unix(commits[0].TS-86400, 0)
		to := time.Unix(commits[len(commits)-1].TS, 0)
		cf.DateFrom, cf.DateTo = &from, &to
		where, wargs := cf.where("", "ts")
		calls, err := s.q.QueryContext(ctx, `SELECT ts, cost_usd FROM call`+where+` ORDER BY ts`, wargs...)
		if err != nil {
			return nil, fmt.Errorf("commit costs: %w", err)
		}
		i := 0
		for calls.Next() {
			var ts int64
			var cost float64
			if err := calls.Scan(&ts, &cost); err != nil {
				calls.Close()
				return nil, err
			}
			for i < len(commits) && commits[i].TS < ts {
				i++
			}
			if i == len(commits) {
				break
			}
			commits[i].Cost += cost
			commits[i].Calls++
		}
		calls.Close()
		if err := calls.Err(); err != nil {
			return nil, err
		}
		for _, c := range commits {
			if c.Calls > 0 && (f.DateFrom == nil || c.TS >= f.DateFrom.Unix()) {
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RateLimitRow is one time an agent was refused for its rate or usage limit.
type RateLimitRow struct {
	SessionUID, Agent, Machine, Detail string
	TS                                 int64
}

// RateLimits lists rate-limit refusals in the filter's agents, machines and
// dates, newest first.
func (s *Store) RateLimits(ctx context.Context, f Filter, limit int) ([]RateLimitRow, int, error) {
	// The table has no model or project column; those axes do not apply.
	f.Models, f.Projects, f.UnpricedOnly = nil, nil, false
	where, args := f.where("", "ts")
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM rate_limit`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("rate limits: %w", err)
	}
	rows, err := s.q.QueryContext(ctx, `SELECT session_uid, ts, agent, machine, detail FROM rate_limit`+where+` ORDER BY ts DESC LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, 0, fmt.Errorf("rate limits: %w", err)
	}
	defer rows.Close()
	var out []RateLimitRow
	for rows.Next() {
		var r RateLimitRow
		if err := rows.Scan(&r.SessionUID, &r.TS, &r.Agent, &r.Machine, &r.Detail); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}
