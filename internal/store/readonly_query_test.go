package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

func TestQueryReadOnlyAndSchema(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	err = st.ReplaceSession(model.SessionWrite{
		Session: model.Session{
			UID:     "sess-q1",
			Agent:   model.AgentClaude,
			Project: "/proj/alpha",
			Machine: "laptop",
			Title:   "test query; semicolon in data",
		},
		Calls: []model.Call{
			{
				SessionUID:   "sess-q1",
				CallKey:      "c1",
				Agent:        model.AgentClaude,
				Project:      "/proj/alpha",
				Machine:      "laptop",
				Model:        "claude-sonnet-4-6",
				Time:         now,
				Day:          now.Format("2006-01-02"),
				InputTokens:  1000,
				OutputTokens: 200,
				TotalTokens:  1200,
				CostUSD:      0.015,
				Priced:       true,
			},
			{
				SessionUID:   "sess-q1",
				CallKey:      "c2",
				Agent:        model.AgentClaude,
				Project:      "/proj/alpha",
				Machine:      "laptop",
				Model:        "claude-sonnet-4-6",
				Time:         now.Add(time.Minute),
				Day:          now.Format("2006-01-02"),
				InputTokens:  500,
				OutputTokens: 100,
				TotalTokens:  600,
				CostUSD:      0.008,
				Priced:       true,
			},
		},
	})
	if err != nil {
		t.Fatalf("ReplaceSession: %v", err)
	}

	ctx := context.Background()

	// Schema includes core tables and their columns.
	tables, err := st.Schema(ctx)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	foundCall := false
	for _, tbl := range tables {
		if tbl.Name == "call" {
			foundCall = true
			if len(tbl.Columns) == 0 {
				t.Errorf("call table has no columns in Schema")
			}
		}
	}
	if !foundCall {
		t.Fatalf("Schema missing 'call' table: %+v", tables)
	}

	// Basic SELECT with trailing semicolon and string containing semicolon.
	res, err := st.QueryReadOnly(ctx, `/* comment */ SELECT call_key, model, cost_usd FROM call WHERE session_uid != 'a;b' ORDER BY call_key;`, 10)
	if err != nil {
		t.Fatalf("QueryReadOnly SELECT: %v", err)
	}
	if res.RowCount != 2 || res.Truncated {
		t.Fatalf("got rows=%d truncated=%v, want 2 false", res.RowCount, res.Truncated)
	}
	if len(res.Columns) != 3 || res.Columns[0] != "call_key" {
		t.Fatalf("unexpected columns: %v", res.Columns)
	}

	// Row cap truncation.
	res, err = st.QueryReadOnly(ctx, `SELECT call_key FROM call ORDER BY call_key`, 1)
	if err != nil {
		t.Fatalf("QueryReadOnly cap: %v", err)
	}
	if res.RowCount != 1 || !res.Truncated {
		t.Fatalf("cap: got rows=%d truncated=%v, want 1 true", res.RowCount, res.Truncated)
	}

	// WITH CTE works.
	res, err = st.QueryReadOnly(ctx, `-- leading comment
WITH totals AS (SELECT COUNT(*) AS n FROM call) SELECT n FROM totals`, 10)
	if err != nil {
		t.Fatalf("QueryReadOnly WITH: %v", err)
	}
	if res.RowCount != 1 {
		t.Fatalf("WITH: got %d rows, want 1", res.RowCount)
	}

	// Reject mutating statements and multi-statement payloads.
	bad := []string{
		"",
		"   ; ",
		"DELETE FROM call",
		"DROP TABLE call",
		"UPDATE call SET cost_usd = 0",
		"INSERT INTO call(session_uid, call_key, agent) VALUES('x','y','z')",
		"PRAGMA user_version = 99",
		"ATTACH DATABASE ':memory:' AS other",
		"SELECT 1; DELETE FROM call",
	}
	for _, q := range bad {
		if _, err := st.QueryReadOnly(ctx, q, 10); err == nil {
			t.Errorf("QueryReadOnly(%q) succeeded, want error", q)
		}
	}

	// Normal writes still work on the pool after QueryReadOnly finishes
	// (proves PRAGMA query_only was reset).
	if err := st.SetMeta("after_query_test", "ok"); err != nil {
		t.Fatalf("SetMeta after QueryReadOnly failed (query_only leaked?): %v", err)
	}
	got, err := st.Meta("after_query_test")
	if err != nil || !strings.EqualFold(got, "ok") {
		t.Fatalf("Meta = %q, %v; want ok", got, err)
	}
}
