package web

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/source"
)

const (
	maxIngestBytes    = 32 << 20 // 32 MiB
	maxIngestSessions = 500
)

// handleIngest accepts parsed sessions pushed by remote `dashd push` clients.
// It is protected by WithAuth when -auth-token is configured.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxIngestBytes)
	var body io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "invalid gzip body", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = io.LimitReader(gz, maxIngestBytes)
	}

	var req model.IngestRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Sessions) > maxIngestSessions {
		http.Error(w, fmt.Sprintf("too many sessions in one request (max %d)", maxIngestSessions), http.StatusRequestEntityTooLarge)
		return
	}

	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()

	ingested := 0
	callsTotal := 0
	for _, item := range req.Sessions {
		if item.Session.UID == "" {
			http.Error(w, "session uid is required", http.StatusBadRequest)
			return
		}
		if item.Session.Machine == "" && req.Machine != "" {
			item.Session.Machine = req.Machine
		}
		var err error
		switch item.Mode {
		case "replace":
			err = s.store.ReplaceSession(item.Session)
		case "append":
			err = s.store.AppendSession(item.Session)
		default:
			http.Error(w, fmt.Sprintf("unknown mode %q", item.Mode), http.StatusBadRequest)
			return
		}
		if err != nil {
			s.log.Error("ingest write failed", "uid", item.Session.UID, "error", err)
			http.Error(w, "ingest failed", http.StatusInternalServerError)
			return
		}
		if err := s.store.RecomputeSession(item.Session.UID); err != nil {
			s.log.Error("ingest recompute failed", "uid", item.Session.UID, "error", err)
			http.Error(w, "ingest failed", http.StatusInternalServerError)
			return
		}
		ingested++
		callsTotal += len(item.Session.Calls)
	}
	if req.Machine != "" {
		if err := s.store.RecordMachineHeartbeat(req.Machine, ingested, callsTotal, time.Now()); err != nil {
			s.log.Warn("record machine heartbeat failed", "machine", req.Machine, "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"ok":       true,
		"ingested": ingested,
	}
	if etag := source.ModelsFileETag(s.ModelsPath); etag != "" {
		resp["models_etag"] = etag
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handleModels serves the server's current models.json so remote push clients
// can stay in sync with the server's pricing table.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ModelsPath == "" {
		http.NotFound(w, r)
		return
	}
	etag := source.ModelsFileETag(s.ModelsPath)
	if etag == "" {
		http.NotFound(w, r)
		return
	}
	quoted := `"` + etag + `"`
	w.Header().Set("ETag", quoted)
	w.Header().Set("Cache-Control", "no-cache")
	if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm == quoted || inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, err := os.ReadFile(s.ModelsPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(data)
}
