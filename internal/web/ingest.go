package web

import (
	"bytes"
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
	"github.com/tonym128/dashd/internal/store"
)

const (
	maxIngestBytes    = 32 << 20 // 32 MiB
	maxIngestSessions = 500
)

// validMachine limits machine names to what a hostname can look like, so a
// client cannot write arbitrary strings into every row and the Machines panel.
func validMachine(m string) bool {
	if m == "" || len(m) > 64 {
		return false
	}
	for _, r := range m {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == ' ':
		default:
			return false
		}
	}
	return true
}

// handleIngest accepts parsed sessions pushed by remote `dashd push` clients.
//
// The server prices every call itself and derives the calendar day from the
// call's timestamp in the server's timezone. Neither figure sent by the client
// is trusted: clients run different price tables and live in different
// timezones, and the dashboard's totals have to be computed one way.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.IngestEnabled {
		http.Error(w, "ingest is disabled on this server; start it with -accept-ingest or an -ingest-token", http.StatusForbidden)
		return
	}
	bound := BoundMachine(r.Context())
	limitKey := "ip:" + clientIP(r, s.TrustProxy)
	if bound != "" {
		limitKey = "machine:" + bound
	}
	if !s.ingestLimiter.allow(limitKey, time.Now()) {
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
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	if len(raw) > maxIngestBytes {
		http.Error(w, "decompressed body too large", http.StatusRequestEntityTooLarge)
		return
	}

	var req model.IngestRequest
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if len(req.Sessions) > maxIngestSessions {
		http.Error(w, fmt.Sprintf("too many sessions in one request (max %d)", maxIngestSessions), http.StatusRequestEntityTooLarge)
		return
	}
	// A machine-bound token decides the machine; nothing in the body can
	// override it.
	if bound != "" {
		req.Machine = bound
	}
	if req.Machine != "" && !validMachine(req.Machine) {
		http.Error(w, "invalid machine name (letters, digits, space, . _ -; max 64)", http.StatusBadRequest)
		return
	}

	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()

	items := make([]store.IngestItem, 0, len(req.Sessions))
	callsTotal := 0
	for _, item := range req.Sessions {
		sess := item.Session
		if sess.UID == "" {
			http.Error(w, "session uid is required", http.StatusBadRequest)
			return
		}
		if bound != "" || sess.Machine == "" {
			sess.Machine = req.Machine
		}
		if sess.Machine != "" && !validMachine(sess.Machine) {
			http.Error(w, "invalid machine name in session", http.StatusBadRequest)
			return
		}
		var replace bool
		switch item.Mode {
		case "replace":
			replace = true
		case "append":
		default:
			http.Error(w, fmt.Sprintf("unknown mode %q", item.Mode), http.StatusBadRequest)
			return
		}
		uid, err := s.store.ResolveIngestUID(sess.UID, sess.Machine)
		if err != nil {
			s.log.Error("ingest uid lookup failed", "uid", sess.UID, "error", err)
			http.Error(w, "ingest failed", http.StatusInternalServerError)
			return
		}
		s.normaliseIngested(&sess, uid)
		items = append(items, store.IngestItem{Replace: replace, Session: sess})
		callsTotal += len(sess.Calls)
	}
	orphaned := make([]string, 0, len(req.Orphaned))
	for _, uid := range req.Orphaned {
		resolved, err := s.store.ResolveIngestUID(uid, req.Machine)
		if err == nil {
			orphaned = append(orphaned, resolved)
		}
	}

	if err := s.store.ApplyIngest(items, orphaned); err != nil {
		s.log.Error("ingest write failed", "machine", req.Machine, "sessions", len(items), "error", err)
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	s.ingestCalls.Add(int64(callsTotal))
	s.ingestRequests.Add(1)
	s.bumpGeneration()
	if req.Machine != "" {
		if err := s.store.RecordMachineHeartbeat(store.Heartbeat{
			Machine: req.Machine, SessionsPushed: len(items), CallsPushed: callsTotal,
			At: time.Now(), Error: req.LastError, Version: req.Version,
		}); err != nil {
			s.log.Warn("record machine heartbeat failed", "machine", req.Machine, "error", err)
		}
	}

	resp := model.IngestResponse{OK: true, Ingested: len(items), ServerVersion: s.Version}
	if id, err := s.store.InstanceID(); err == nil {
		resp.InstanceID = id
	}
	resp.ModelsETag = source.ModelsFileETag(s.ModelsPath)
	writeJSON(w, resp)
}

// normaliseIngested rewrites a pushed session for storage: its uid (possibly
// namespaced, see store.ResolveIngestUID), and every call's cost and day.
func (s *Server) normaliseIngested(sess *model.SessionWrite, uid string) {
	sess.UID = uid
	for i := range sess.Calls {
		c := &sess.Calls[i]
		c.SessionUID = uid
		if c.Machine == "" || c.Machine != sess.Machine {
			c.Machine = sess.Machine
		}
		if c.Time.IsZero() {
			c.Day = ""
		} else {
			c.Day = c.Time.Local().Format("2006-01-02")
		}
		if s.Pricer != nil {
			var ts int64
			if !c.Time.IsZero() {
				ts = c.Time.Unix()
			}
			c.CostUSD, c.Priced = s.Pricer(c.Model, int64(c.InputTokens), int64(c.OutputTokens+c.ReasoningTokens),
				int64(c.CacheReadTokens), int64(c.CacheWriteTokens), int64(c.CacheWrite1hTokens), ts)
		}
	}
	for i := range sess.ToolCalls {
		sess.ToolCalls[i].SessionUID = uid
		sess.ToolCalls[i].Machine = sess.Machine
	}
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
	if !adminAllowed(r) {
		http.Error(w, "admin access required", http.StatusForbidden)
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
	s.bumpGeneration()
	writeJSON(w, map[string]any{
		"ok":       true,
		"repriced": n,
	})
}
