package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
)

func TestIngestEndpointAuthAndGzip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv, err := New(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := WithAuth("secret-token", nil, srv.Handler())

	payload := model.IngestRequest{
		Machine: "laptop-1",
		Sessions: []model.IngestSession{{
			Mode: "replace",
			Session: model.SessionWrite{
				Session: model.Session{
					UID:     "remote-sess-1",
					Agent:   model.AgentClaude,
					Project: "/home/tony/proj",
				},
				Path: "/home/tony/.claude/projects/proj/remote-sess-1.jsonl",
				Calls: []model.Call{{
					SessionUID:   "remote-sess-1",
					Agent:        model.AgentClaude,
					Project:      "/home/tony/proj",
					Model:        "claude-sonnet-4-6",
					CallKey:      "call-1",
					Time:         time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
					InputTokens:  1000,
					OutputTokens: 200,
					TotalTokens:  1200,
					CostUSD:      0.42,
					Priced:       true,
				}},
			},
		}},
	}

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if err := json.NewEncoder(gz).Encode(payload); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	// Unauthenticated request is rejected.
	reqUnauth := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(gzBuf.Bytes()))
	recUnauth := httptest.NewRecorder()
	handler.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", recUnauth.Code)
	}

	// Authenticated gzipped request succeeds.
	req := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(gzBuf.Bytes()))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, body = %s", rec.Code, rec.Body.String())
	}

	machines, err := st.Machines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || machines[0] != "laptop-1" {
		t.Fatalf("Machines() = %v, want [laptop-1]", machines)
	}

	// Filtering the index by ?machine=laptop-1 renders the machine in the page.
	idxReq := httptest.NewRequest(http.MethodGet, "/?machine=laptop-1", nil)
	idxReq.Header.Set("Authorization", "Bearer secret-token")
	idxRec := httptest.NewRecorder()
	handler.ServeHTTP(idxRec, idxReq)
	if idxRec.Code != http.StatusOK {
		t.Fatalf("GET /?machine=laptop-1 status = %d", idxRec.Code)
	}
	if !strings.Contains(idxRec.Body.String(), "laptop-1") {
		t.Errorf("expected rendered index to contain laptop-1")
	}
}

func TestSplitAuthAndModelsSync(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	modelsFile := filepath.Join(dir, "models.json")
	if err := os.WriteFile(modelsFile, []byte(`{"data":[{"id":"google/gemini-4-hydrogen","pricing":{"prompt":"0.0000015","completion":"0.0000075"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, err := New(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.ModelsPath = modelsFile
	handler := WithSplitAuth("ui-secret", "ingest-secret", nil, srv.Handler())

	// Ingest token cannot read the dashboard UI, but can read /api/models.
	uiReq := httptest.NewRequest(http.MethodGet, "/", nil)
	uiReq.Header.Set("Authorization", "Bearer ingest-secret")
	uiRec := httptest.NewRecorder()
	handler.ServeHTTP(uiRec, uiReq)
	if uiRec.Code != http.StatusUnauthorized {
		t.Fatalf("ingest token on UI = %d, want 401", uiRec.Code)
	}

	mReq := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	mReq.Header.Set("Authorization", "Bearer ingest-secret")
	mRec := httptest.NewRecorder()
	handler.ServeHTTP(mRec, mReq)
	if mRec.Code != http.StatusOK {
		t.Fatalf("GET /api/models = %d, want 200", mRec.Code)
	}
	etag := mRec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header from /api/models")
	}

	mReq304 := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	mReq304.Header.Set("Authorization", "Bearer ingest-secret")
	mReq304.Header.Set("If-None-Match", etag)
	mRec304 := httptest.NewRecorder()
	handler.ServeHTTP(mRec304, mReq304)
	if mRec304.Code != http.StatusNotModified {
		t.Fatalf("GET /api/models with If-None-Match = %d, want 304", mRec304.Code)
	}

	// POST /api/reprice requires UI auth and calls RepriceFunc.
	srv.RepriceFunc = func(context.Context) (int, error) { return 42, nil }
	repUnauth := httptest.NewRequest(http.MethodPost, "/api/reprice", nil)
	repUnauth.Header.Set("Authorization", "Bearer ingest-secret")
	repUnauthRec := httptest.NewRecorder()
	handler.ServeHTTP(repUnauthRec, repUnauth)
	if repUnauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("ingest token on /api/reprice = %d, want 401", repUnauthRec.Code)
	}

	repReq := httptest.NewRequest(http.MethodPost, "/api/reprice", nil)
	repReq.Header.Set("Authorization", "Bearer ui-secret")
	repRec := httptest.NewRecorder()
	handler.ServeHTTP(repRec, repReq)
	if repRec.Code != http.StatusOK || !strings.Contains(repRec.Body.String(), `"repriced":42`) {
		t.Fatalf("POST /api/reprice = %d %s", repRec.Code, repRec.Body.String())
	}

	// ?unpriced=1 filter renders cleanly.
	uReq := httptest.NewRequest(http.MethodGet, "/?unpriced=1", nil)
	uReq.Header.Set("Authorization", "Bearer ui-secret")
	uRec := httptest.NewRecorder()
	handler.ServeHTTP(uRec, uReq)
	if uRec.Code != http.StatusOK {
		t.Fatalf("GET /?unpriced=1 = %d", uRec.Code)
	}

	// GET /metrics returns Prometheus text format.
	metReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metReq.Header.Set("Authorization", "Bearer ui-secret")
	metRec := httptest.NewRecorder()
	handler.ServeHTTP(metRec, metReq)
	if metRec.Code != http.StatusOK || !strings.Contains(metRec.Body.String(), "dashd_calls ") {
		t.Fatalf("GET /metrics = %d %s", metRec.Code, metRec.Body.String())
	}

	// GET /api/sessions returns paginated JSON.
	sessReq := httptest.NewRequest(http.MethodGet, "/api/sessions?limit=10&offset=0", nil)
	sessReq.Header.Set("Authorization", "Bearer ui-secret")
	sessRec := httptest.NewRecorder()
	handler.ServeHTTP(sessRec, sessReq)
	if sessRec.Code != http.StatusOK || !strings.Contains(sessRec.Body.String(), `"sessions"`) {
		t.Fatalf("GET /api/sessions = %d %s", sessRec.Code, sessRec.Body.String())
	}
}
