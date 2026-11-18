package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/source"
)

const (
	maxIngestBytes    = 32 << 20 // 32 MiB
	maxIngestSessions = 500
)

type ipBucket struct {
	tokens float64
	last   time.Time
}

type ingestRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
}

func (l *ingestRateLimiter) allow(remoteAddr string, now time.Time) bool {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		ip = remoteAddr
	}
	const rate = 60.0 // tokens per second
	const burst = 200.0

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = make(map[string]*ipBucket)
	}
	// Evict stale entries occasionally to keep memory bounded.
	if len(l.buckets) > 1024 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[ip]
	if !ok {
		l.buckets[ip] = &ipBucket{tokens: burst - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(burst, b.tokens+elapsed*rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// handleIngest accepts parsed sessions pushed by remote `dashd push` clients.
// It is protected by WithAuth when -auth-token is configured.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.ingestLimiter.allow(r.RemoteAddr, time.Now()) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
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
		body = gz
	}

	raw, err := io.ReadAll(io.LimitReader(body, maxIngestBytes+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("read body: %v", err), http.StatusBadRequest)
		return
	}
	if len(raw) > maxIngestBytes {
		http.Error(w, "decompressed body too large", http.StatusRequestEntityTooLarge)
		return
	}

	var req model.IngestRequest
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&req); err != nil {
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

func (s *Server) handleReprice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.RepriceFunc == nil {
		http.Error(w, "repricing not configured", http.StatusNotImplemented)
		return
	}
	n, err := s.RepriceFunc(r.Context())
	if err != nil {
		s.log.Error("manual reprice failed", "error", err)
		http.Error(w, "reprice failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"repriced": n,
	})
}
