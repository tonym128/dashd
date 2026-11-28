package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPHTTP(t *testing.T) {
	srv, st := newTestServer(t)
	srv.Version = "0.11.0-test"
	seed(t, st, time.Now())

	h := WithAuthConfig(AuthConfig{
		UIToken:     "ui-tok",
		ReadToken:   "read-tok",
		IngestToken: "ingest-tok",
		AdminToken:  "admin-tok",
		Log:         discardLog(),
	}, srv.Handler())

	call := func(token, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(payload))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Auth checks: no token and ingest token rejected; read token allowed.
	if rec := call("", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token /mcp: got %d, want 401", rec.Code)
	}
	if rec := call("ingest-tok", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ingest token /mcp: got %d, want 401", rec.Code)
	}

	// initialize
	rec := call("read-tok", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize: got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"dashd"`) || !strings.Contains(rec.Body.String(), `"0.11.0-test"`) {
		t.Errorf("initialize unexpected body: %s", rec.Body.String())
	}

	// notifications/initialized -> 202 Accepted, empty body.
	rec = call("read-tok", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notifications/initialized: got %d, want 202", rec.Code)
	}

	// ping
	rec = call("read-tok", `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("ping: got %d", rec.Code)
	}

	// tools/list
	rec = call("read-tok", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list: got %d", rec.Code)
	}
	for _, name := range []string{"get_summary", "list_sessions", "get_insights", "get_digest", "get_schema", "query_sql"} {
		if !strings.Contains(rec.Body.String(), `"`+name+`"`) {
			t.Errorf("tools/list missing %q", name)
		}
	}

	// Exercise each tool.
	toolCalls := []struct {
		name      string
		args      string
		wantSub   string
		wantError bool
	}{
		{"get_summary", `{"range":"30d"}`, `"models"`, false},
		{"list_sessions", `{"limit":5}`, `"sessions"`, false},
		{"get_insights", `{}`, `[`, false},
		{"get_digest", `{"days":7}`, `dashd`, false},
		{"get_schema", `{}`, `"call"`, false},
		{"query_sql", `{"sql":"SELECT COUNT(*) AS n FROM call","limit":5}`, `"row_count"`, false},
		{"query_sql", `{"sql":"DELETE FROM call"}`, `only SELECT`, true},
	}
	for i, tc := range toolCalls {
		payload := `{"jsonrpc":"2.0","id":` + jsonNumber(10+i) + `,"method":"tools/call","params":{"name":"` + tc.name + `","arguments":` + tc.args + `}}`
		rec = call("read-tok", payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("tool %s: status %d body %s", tc.name, rec.Code, rec.Body.String())
		}
		var resp struct {
			Result mcpToolResult `json:"result"`
			Error  *jsonRPCError `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("tool %s invalid json: %v", tc.name, err)
		}
		if resp.Error != nil {
			t.Fatalf("tool %s unexpected rpc error: %+v", tc.name, resp.Error)
		}
		if resp.Result.IsError != tc.wantError {
			t.Errorf("tool %s IsError=%v, want %v (content=%+v)", tc.name, resp.Result.IsError, tc.wantError, resp.Result.Content)
		}
		if len(resp.Result.Content) == 0 || !strings.Contains(strings.ToLower(resp.Result.Content[0].Text), strings.ToLower(tc.wantSub)) {
			t.Errorf("tool %s content = %+v, want substring %q", tc.name, resp.Result.Content, tc.wantSub)
		}
	}

	// Unknown tool returns JSON-RPC error.
	rec = call("read-tok", `{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"nope"}}`)
	if !strings.Contains(rec.Body.String(), `"unknown tool: nope"`) {
		t.Errorf("unknown tool response: %s", rec.Body.String())
	}

	// Invalid JSON returns parse error.
	rec = call("read-tok", `{not json`)
	if !strings.Contains(rec.Body.String(), `-32700`) {
		t.Errorf("invalid json response: %s", rec.Body.String())
	}
}

func jsonNumber(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
