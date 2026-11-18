package store

import (
	"context"
	"fmt"
	"strings"
)

// SessionQuery pages, sorts and searches the session list in SQL.
type SessionQuery struct {
	// Search matches project, path, agent, title or machine, case-insensitively.
	Search string
	// Sort is a column key from sessionSortColumns; "" means last activity.
	Sort   string
	Asc    bool
	Limit  int
	Offset int
}

var sessionSortColumns = map[string]string{
	"last":     "s.last_ts",
	"first":    "s.first_ts",
	"cost":     "s.cost_usd",
	"tokens":   "s.total_tokens",
	"calls":    "s.calls",
	"wall":     "s.wall_seconds",
	"llm":      "s.llm_seconds",
	"tool":     "s.tool_seconds",
	"project":  "s.project",
	"agent":    "s.agent",
	"machine":  "s.machine",
	"title":    "s.title",
	"messages": "s.calls",
}

// SessionsPage returns one page of sessions and the total number matching.
// Unlike Sessions it has no hidden cap: the database does the paging.
func (s *Store) SessionsPage(ctx context.Context, f Filter, q SessionQuery) ([]SessionRow, int, error) {
	where, args := f.whereSession("s")
	if term := strings.TrimSpace(q.Search); term != "" {
		like := "%" + escapeLike(strings.ToLower(term)) + "%"
		clause := `(LOWER(s.project) LIKE ? ESCAPE '\' OR LOWER(s.path) LIKE ? ESCAPE '\' OR LOWER(s.agent) LIKE ? ESCAPE '\' OR LOWER(s.title) LIKE ? ESCAPE '\' OR LOWER(s.machine) LIKE ? ESCAPE '\')`
		if where == "" {
			where = " WHERE " + clause
		} else {
			where += " AND " + clause
		}
		args = append(args, like, like, like, like, like)
	}
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM session s`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count sessions: %w", err)
	}
	col, ok := sessionSortColumns[q.Sort]
	if !ok {
		col = "s.last_ts"
	}
	dir := "DESC"
	if q.Asc {
		dir = "ASC"
	}
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	offset := max(q.Offset, 0)
	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sessionColumns+`
		FROM session s`+where+`
		ORDER BY `+col+` `+dir+`, s.uid LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("sessions page: %w", err)
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		r, err := scanSessionRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ExportCall is one stored call, as written by `dashd export` and the
// server-side CSV/JSON export.
type ExportCall struct {
	SessionUID       string  `json:"session_uid"`
	CallKey          string  `json:"call_key"`
	Agent            string  `json:"agent"`
	Project          string  `json:"project"`
	Machine          string  `json:"machine"`
	Model            string  `json:"model"`
	TS               int64   `json:"ts"`
	Day              string  `json:"day"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	LLMSeconds       float64 `json:"llm_seconds"`
	CostUSD          float64 `json:"cost_usd"`
	Priced           bool    `json:"priced"`
	IngestedAt       int64   `json:"ingested_at"`
}

// ExportCalls streams every call matching f, oldest first, to fn.
func (s *Store) ExportCalls(ctx context.Context, f Filter, fn func(ExportCall) error) error {
	where, args := f.where("", "ts")
	rows, err := s.q.QueryContext(ctx, `
		SELECT session_uid, call_key, agent, project, machine, model, ts, day,
		       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		       reasoning_tokens, total_tokens, llm_seconds, cost_usd, priced, ingested_at
		FROM call`+where+` ORDER BY ts, session_uid, call_key`, args...)
	if err != nil {
		return fmt.Errorf("export calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c ExportCall
		var priced int
		if err := rows.Scan(&c.SessionUID, &c.CallKey, &c.Agent, &c.Project, &c.Machine, &c.Model,
			&c.TS, &c.Day, &c.InputTokens, &c.OutputTokens, &c.CacheReadTokens, &c.CacheWriteTokens,
			&c.ReasoningTokens, &c.TotalTokens, &c.LLMSeconds, &c.CostUSD, &priced, &c.IngestedAt); err != nil {
			return err
		}
		c.Priced = priced != 0
		if err := fn(c); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SessionTool is one tool invocation on the session page.
type SessionTool struct {
	TS      int64
	Tool    string
	Seconds float64
	IsError bool
}

// SessionTools returns a session's tool calls in time order.
func (s *Store) SessionTools(ctx context.Context, uid string) ([]SessionTool, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT ts, tool, seconds, is_error FROM tool_call WHERE session_uid = ? ORDER BY ts, call_key`, uid)
	if err != nil {
		return nil, fmt.Errorf("session tools %s: %w", uid, err)
	}
	defer rows.Close()
	var out []SessionTool
	for rows.Next() {
		var t SessionTool
		var isErr int
		if err := rows.Scan(&t.TS, &t.Tool, &t.Seconds, &isErr); err != nil {
			return nil, err
		}
		t.IsError = isErr != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// CostSince returns total cost of calls at or after ts (Unix seconds).
func (s *Store) CostSince(ctx context.Context, ts int64) (float64, error) {
	var c float64
	err := s.q.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM call WHERE ts >= ?`, ts).Scan(&c)
	return c, err
}

// CostBy returns cost grouped by machine or agent, for /metrics.
func (s *Store) CostBy(ctx context.Context, col string) (map[string]float64, error) {
	if col != "machine" && col != "agent" {
		return nil, fmt.Errorf("cost by %q is not supported", col)
	}
	rows, err := s.q.QueryContext(ctx, `SELECT `+col+`, COALESCE(SUM(cost_usd), 0) FROM call GROUP BY `+col)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var k string
		var v float64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
