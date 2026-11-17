package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
