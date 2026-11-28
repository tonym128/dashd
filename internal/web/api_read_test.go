package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestReadAPIs(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	h := srv.Handler()

	// GET /api/summary
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary?range=30d", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/summary: status %d, body %s", rec.Code, rec.Body.String())
	}
	var summary map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("/api/summary invalid json: %v", err)
	}
	for _, key := range []string{"models", "projects", "dailyStats", "burn", "facets", "insights"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("/api/summary missing key %q", key)
		}
	}

	// GET /api/insights
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/insights", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/insights: status %d", rec.Code)
	}
	var insights []Insight
	if err := json.Unmarshal(rec.Body.Bytes(), &insights); err != nil {
		t.Fatalf("/api/insights invalid json: %v", err)
	}

	// GET /api/digest (JSON and markdown)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/digest?days=7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/digest: status %d", rec.Code)
	}
	var dig map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &dig); err != nil {
		t.Fatalf("/api/digest invalid json: %v", err)
	}
	if _, ok := dig["markdown"].(string); !ok {
		t.Errorf("/api/digest missing markdown string: %+v", dig)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/digest?days=7&format=markdown", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("/api/digest markdown: status %d ct %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	// GET /api/stats
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/stats: status %d", rec.Code)
	}
	var stats map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("/api/stats invalid json: %v", err)
	}
	if stats["calls"].(float64) < 1 {
		t.Errorf("/api/stats calls = %v, want >= 1", stats["calls"])
	}

	// GET /api/schema
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/schema", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/schema: status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"call"`) {
		t.Errorf("/api/schema missing call table: %s", rec.Body.String())
	}

	// POST /api/query
	body := bytes.NewBufferString(`{"sql":"SELECT COUNT(*) AS n FROM call","limit":10}`)
	req := httptest.NewRequest(http.MethodPost, "/api/query", body)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/query: status %d body %s", rec.Code, rec.Body.String())
	}
	var qres map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &qres); err != nil {
		t.Fatalf("POST /api/query invalid json: %v", err)
	}
	if qres["row_count"].(float64) != 1 {
		t.Errorf("POST /api/query row_count = %v, want 1", qres["row_count"])
	}

	// GET /api/query
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/query?sql="+url.QueryEscape("SELECT COUNT(*) FROM call"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/query: status %d body %s", rec.Code, rec.Body.String())
	}

	// Mutating SQL rejected with 400.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewBufferString(`{"sql":"DELETE FROM call"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("DELETE via /api/query: status %d, want 400", rec.Code)
	}
}

func TestReadOnlyTokenRole(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	h := WithAuthConfig(AuthConfig{
		UIToken:     "ui-secret",
		ReadToken:   "read-secret",
		IngestToken: "ingest-secret",
		AdminToken:  "admin-secret",
		Log:         discardLog(),
	}, srv.Handler())

	do := func(method, path, token, body string) int {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	// Read token can access all read endpoints, /api/query, and /api/models.
	for _, path := range []string{"/", "/api/summary", "/api/insights", "/api/digest", "/api/stats", "/api/schema", "/api/sessions", "/api/export?kind=calls&format=json"} {
		if code := do(http.MethodGet, path, "read-secret", ""); code != http.StatusOK {
			t.Errorf("read token GET %s = %d, want 200", path, code)
		}
	}
	if code := do(http.MethodPost, "/api/query", "read-secret", `{"sql":"SELECT 1"}`); code != http.StatusOK {
		t.Errorf("read token POST /api/query = %d, want 200", code)
	}

	// Read token MUST NOT ingest or run admin actions.
	if code := do(http.MethodPost, "/api/ingest", "read-secret", `{"machine":"m","sessions":[]}`); code != http.StatusUnauthorized {
		t.Errorf("read token POST /api/ingest = %d, want 401", code)
	}
	if code := do(http.MethodPost, "/api/admin/prune", "read-secret", `{}`); code != http.StatusUnauthorized {
		t.Errorf("read token POST /api/admin/prune = %d, want 401", code)
	}
	if code := do(http.MethodPost, "/api/reprice", "read-secret", `{}`); code != http.StatusUnauthorized {
		t.Errorf("read token POST /api/reprice = %d, want 401", code)
	}

	// Ingest token MUST NOT read /api/summary or /api/query.
	if code := do(http.MethodGet, "/api/summary", "ingest-secret", ""); code != http.StatusUnauthorized {
		t.Errorf("ingest token GET /api/summary = %d, want 401", code)
	}
	if code := do(http.MethodPost, "/api/query", "ingest-secret", `{"sql":"SELECT 1"}`); code != http.StatusUnauthorized {
		t.Errorf("ingest token POST /api/query = %d, want 401", code)
	}
}
