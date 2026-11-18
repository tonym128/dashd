package web

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/tonym128/dashd/internal/store"
)

// Admin endpoints. Every one is POST, needs RoleAdmin when a token is
// configured (enforced by the auth middleware) and a loopback client when none
// is (adminAllowed), and is refused cross-site by withRequestGuards.

func (s *Server) adminRequest(w http.ResponseWriter, r *http.Request, into any) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !adminAllowed(r) {
		http.Error(w, "admin access required", http.StatusForbidden)
		return false
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(into); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) handleAdminPrune(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Before         string `json:"before"`
		IncludeUndated bool   `json:"include_undated"`
		Vacuum         bool   `json:"vacuum"`
	}
	if !s.adminRequest(w, r, &req) {
		return
	}
	cutoff, err := time.ParseInLocation("2006-01-02", req.Before, time.Local)
	if err != nil {
		http.Error(w, "before must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	prune := s.PruneFunc
	if prune == nil {
		prune = s.store.PruneBefore
	}
	n, err := prune(r.Context(), cutoff, store.PruneOptions{IncludeUndated: req.IncludeUndated, Vacuum: req.Vacuum})
	if err != nil {
		s.log.Error("admin prune failed", "error", err)
		http.Error(w, "prune failed", http.StatusInternalServerError)
		return
	}
	s.bumpGeneration()
	s.log.Info("admin prune", "before", req.Before, "calls", n)
	writeJSON(w, map[string]any{"ok": true, "pruned": n})
}

func (s *Server) handleAdminForgetSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	if !s.adminRequest(w, r, &req) {
		return
	}
	ok, err := s.store.SessionExists(req.UID)
	if err != nil {
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if err := s.store.ForgetSession(req.UID); err != nil {
		s.log.Error("admin forget session failed", "uid", req.UID, "error", err)
		http.Error(w, "forget failed", http.StatusInternalServerError)
		return
	}
	s.bumpGeneration()
	s.log.Info("admin forgot session", "uid", req.UID)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAdminRenameMachine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !s.adminRequest(w, r, &req) {
		return
	}
	if !validMachine(req.To) {
		http.Error(w, "invalid target machine name", http.StatusBadRequest)
		return
	}
	n, err := s.store.RenameMachine(req.From, req.To)
	if err != nil {
		http.Error(w, "rename failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.bumpGeneration()
	s.log.Info("admin renamed machine", "from", req.From, "to", req.To, "calls", n)
	writeJSON(w, map[string]any{"ok": true, "calls": n})
}
