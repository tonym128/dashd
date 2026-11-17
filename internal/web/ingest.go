package web

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tonym128/dashd/internal/model"
)

const maxIngestBytes = 32 << 20 // 32 MiB

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
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}

	ingested := 0
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
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"ingested": ingested,
	})
}
