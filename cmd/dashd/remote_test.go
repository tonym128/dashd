package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
	"github.com/tonym128/dashd/internal/web"
)

func seedRemoteTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	err = st.ReplaceSession(model.SessionWrite{
		Session: model.Session{
			UID:     "sess-r1",
			Agent:   model.AgentClaude,
			Project: "/proj/remote-test",
			Machine: "workstation",
			Title:   "remote cli test",
		},
		Calls: []model.Call{
			{
				SessionUID:   "sess-r1",
				CallKey:      "c1",
				Agent:        model.AgentClaude,
				Project:      "/proj/remote-test",
				Machine:      "workstation",
				Model:        "claude-sonnet-4-6",
				Time:         now,
				Day:          now.Format("2006-01-02"),
				InputTokens:  1200,
				OutputTokens: 300,
				TotalTokens:  1500,
				LLMSeconds:   2.5,
				CostUSD:      0.025,
				Priced:       true,
			},
		},
	})
	if err != nil {
		t.Fatalf("ReplaceSession: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := web.New(st, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	srv.Version = "test"
	h := web.WithAuthConfig(web.AuthConfig{
		UIToken:   "ui-secret",
		ReadToken: "read-secret",
		Log:       log,
	}, srv.Handler())
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, st
}

func TestRemoteCLIAndQuery(t *testing.T) {
	ts, st := seedRemoteTestServer(t)
	ctx := context.Background()
	base := options{
		remote:     ts.URL,
		readToken:  "read-secret",
		queryLimit: 100,
		format:     "json",
	}

	// Remote stats (text & JSON)
	var out bytes.Buffer
	if err := runRemoteStats(ctx, base, &out); err != nil {
		t.Fatalf("runRemoteStats: %v", err)
	}
	if !strings.Contains(out.String(), "calls:    1") {
		t.Errorf("stats text = %q", out.String())
	}

	out.Reset()
	jopt := base
	jopt.jsonOut = true
	if err := runRemoteStats(ctx, jopt, &out); err != nil {
		t.Fatalf("runRemoteStats json: %v", err)
	}
	if !strings.Contains(out.String(), `"calls"`) {
		t.Errorf("stats json = %q", out.String())
	}

	// Remote insights
	out.Reset()
	if err := runRemoteInsights(ctx, jopt, &out); err != nil {
		t.Fatalf("runRemoteInsights: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "[") {
		t.Errorf("insights json = %q", out.String())
	}

	// Remote digest
	out.Reset()
	if err := runRemoteDigest(ctx, base, &out); err != nil {
		t.Fatalf("runRemoteDigest: %v", err)
	}
	if !strings.Contains(out.String(), "dashd") {
		t.Errorf("digest = %q", out.String())
	}

	// Remote export
	out.Reset()
	if err := runRemoteExport(ctx, base, &out); err != nil {
		t.Fatalf("runRemoteExport: %v", err)
	}
	if !strings.Contains(out.String(), "claude-sonnet-4-6") {
		t.Errorf("export = %q", out.String())
	}

	// Remote query (-sql and -schema)
	qopt := base
	qopt.sqlQuery = "SELECT model, cost_usd FROM call"
	out.Reset()
	if err := runQuery(ctx, qopt, nil, &out); err != nil {
		t.Fatalf("runQuery remote: %v", err)
	}
	if !strings.Contains(out.String(), "claude-sonnet-4-6") {
		t.Errorf("query table = %q", out.String())
	}

	qopt.showSchema = true
	qopt.jsonOut = true
	out.Reset()
	if err := runQuery(ctx, qopt, nil, &out); err != nil {
		t.Fatalf("runQuery remote schema: %v", err)
	}
	if !strings.Contains(out.String(), `"call"`) {
		t.Errorf("schema json = %q", out.String())
	}

	// Local query against store directly
	lopt := options{sqlQuery: "SELECT COUNT(*) AS n FROM call", queryLimit: 10, jsonOut: true}
	out.Reset()
	if err := runQuery(ctx, lopt, st, &out); err != nil {
		t.Fatalf("runQuery local: %v", err)
	}
	var qres store.QueryResult
	if err := json.Unmarshal(out.Bytes(), &qres); err != nil {
		t.Fatalf("local query json: %v", err)
	}
	if qres.RowCount != 1 {
		t.Errorf("local query row_count = %d, want 1", qres.RowCount)
	}
}

func TestMCPStdioLocalAndRemote(t *testing.T) {
	ts, st := seedRemoteTestServer(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query_sql","arguments":{"sql":"SELECT model FROM call"}}}`,
	}, "\n") + "\n"

	// Local stdio
	var localOut bytes.Buffer
	if err := runMCP(ctx, options{}, st, strings.NewReader(input), &localOut, log); err != nil {
		t.Fatalf("runMCP local: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(localOut.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("local MCP: got %d lines (%q), want 2", len(lines), localOut.String())
	}
	if !strings.Contains(lines[1], "claude-sonnet-4-6") {
		t.Errorf("local MCP query output = %s", lines[1])
	}

	// Remote stdio
	var remoteOut bytes.Buffer
	if err := runMCP(ctx, options{remote: ts.URL, readToken: "read-secret"}, nil, strings.NewReader(input), &remoteOut, log); err != nil {
		t.Fatalf("runMCP remote: %v", err)
	}
	lines = strings.Split(strings.TrimSpace(remoteOut.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("remote MCP: got %d lines (%q), want 2", len(lines), remoteOut.String())
	}
	if !strings.Contains(lines[1], "claude-sonnet-4-6") {
		t.Errorf("remote MCP query output = %s", lines[1])
	}
}
