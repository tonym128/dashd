package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultQueryMaxRows is the row limit applied when a caller does not ask
	// for one, keeping ad-hoc LLM and API responses bounded.
	DefaultQueryMaxRows = 500
	// HardQueryMaxRows is the upper bound no caller may exceed.
	HardQueryMaxRows = 5000
	// DefaultQueryTimeout bounds how long a read-only SQL query may run when
	// the caller's context carries no deadline.
	DefaultQueryTimeout = 10 * time.Second
)

// QueryResult is the tabular result of a read-only SQL query.
type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated,omitempty"`
}

// ColumnSchema describes one column in a database table or view.
type ColumnSchema struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null,omitempty"`
	PK      bool   `json:"pk,omitempty"`
}

// TableSchema describes one table or view in the database.
type TableSchema struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	SQL     string         `json:"sql"`
	Columns []ColumnSchema `json:"columns"`
}

// Schema returns the schema of every user table and view in the database.
func (s *Store) Schema(ctx context.Context) ([]TableSchema, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT name, type, COALESCE(sql, '')
		FROM sqlite_master
		WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []TableSchema
	for rows.Next() {
		var t TableSchema
		if err := rows.Scan(&t.Name, &t.Type, &t.SQL); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		cols, err := s.tableColumns(ctx, out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Columns = cols
	}
	return out, nil
}

func (s *Store) tableColumns(ctx context.Context, name string) ([]ColumnSchema, error) {
	// Table names come from sqlite_master; double-quote escaping handles any
	// identifier safely.
	quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	rows, err := s.q.QueryContext(ctx, "PRAGMA table_info("+quoted+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []ColumnSchema
	for rows.Next() {
		var cid int
		var cname, ctype string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &cname, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, ColumnSchema{
			Name:    cname,
			Type:    ctype,
			NotNull: notNull != 0,
			PK:      pk != 0,
		})
	}
	return cols, rows.Err()
}

// QueryReadOnly executes a single read-only SQL query (SELECT, WITH, or
// EXPLAIN) against the store with SQLite's query_only pragma enforced on the
// connection, a bounded timeout, and a row limit.
func (s *Store) QueryReadOnly(ctx context.Context, query string, maxRows int) (QueryResult, error) {
	cleaned, err := validateReadOnlySQL(query)
	if err != nil {
		return QueryResult{}, err
	}
	if maxRows <= 0 {
		maxRows = DefaultQueryMaxRows
	}
	if maxRows > HardQueryMaxRows {
		maxRows = HardQueryMaxRows
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultQueryTimeout)
		defer cancel()
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return QueryResult{}, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	// Enforce read-only at the SQLite engine level so no statement, CTE, or
	// pragma can modify the database or attach another file.
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return QueryResult{}, fmt.Errorf("enable query_only: %w", err)
	}
	defer func() {
		// Use a fresh short context so cleanup still runs if the query's ctx
		// timed out or was cancelled, before the connection returns to the pool.
		resetCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(resetCtx, "PRAGMA query_only = OFF")
	}()

	rows, err := conn.QueryContext(ctx, cleaned)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return QueryResult{}, err
	}
	res := QueryResult{
		Columns: cols,
		Rows:    make([][]any, 0),
	}

	dest := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range dest {
		ptrs[i] = &dest[i]
	}

	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return QueryResult{}, err
		}
		row := make([]any, len(cols))
		for i, v := range dest {
			if b, ok := v.([]byte); ok {
				row[i] = string(b)
			} else {
				row[i] = v
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, err
	}
	res.RowCount = len(res.Rows)
	return res, nil
}

// validateReadOnlySQL checks that query is a single SELECT, WITH, or EXPLAIN
// statement. SQLite's PRAGMA query_only is the authoritative write barrier;
// this check gives clear errors and rejects multi-statement payloads up front.
func validateReadOnlySQL(q string) (string, error) {
	trimmed := strings.TrimSpace(q)
	if trimmed == "" {
		return "", errors.New("sql query is empty")
	}
	// Strip a single trailing semicolon if present.
	trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, ";"))
	if trimmed == "" {
		return "", errors.New("sql query is empty")
	}
	if hasUnquotedSemicolon(trimmed) {
		return "", errors.New("only a single SQL statement is allowed")
	}

	lead := leadingSQLKeyword(trimmed)
	switch lead {
	case "SELECT", "WITH", "EXPLAIN":
		return trimmed, nil
	default:
		return "", fmt.Errorf("only SELECT, WITH, and EXPLAIN queries are allowed (got %q)", lead)
	}
}

// leadingSQLKeyword skips leading whitespace and SQL comments (-- and /* */)
// and returns the uppercase first identifier.
func leadingSQLKeyword(s string) string {
	i := 0
	n := len(s)
	for i < n {
		// Skip whitespace.
		for i < n && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		// Skip -- line comment.
		if i+1 < n && s[i] == '-' && s[i+1] == '-' {
			i += 2
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		}
		// Skip /* block comment */.
		if i+1 < n && s[i] == '/' && s[i+1] == '*' {
			i += 2
			for i+1 < n && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			} else {
				return ""
			}
			continue
		}
		break
	}
	start := i
	for i < n && ((s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') || s[i] == '_') {
		i++
	}
	return strings.ToUpper(s[start:i])
}

// hasUnquotedSemicolon reports whether s contains a semicolon outside single-
// or double-quoted strings and SQL comments.
func hasUnquotedSemicolon(s string) bool {
	inSingle := false
	inDouble := false
	inLineComment := false
	inBlockComment := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++ // escaped ''
				} else {
					inSingle = false
				}
			}
			continue
		}
		if inDouble {
			if c == '"' {
				if i+1 < len(s) && s[i+1] == '"' {
					i++ // escaped ""
				} else {
					inDouble = false
				}
			}
			continue
		}
		if c == '-' && i+1 < len(s) && s[i+1] == '-' {
			inLineComment = true
			i++
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			inBlockComment = true
			i++
			continue
		}
		if c == '\'' {
			inSingle = true
			continue
		}
		if c == '"' {
			inDouble = true
			continue
		}
		if c == ';' {
			return true
		}
	}
	return false
}
