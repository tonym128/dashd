package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tonym128/dashd/internal/store"
)

const maxQueryBodyBytes = 64 << 10 // 64 KiB

// handleSummaryAPI serves GET /api/summary, returning the dashboard's full
// rollup payload (totals, burn windows, daily series, models, providers,
// projects, machines, tools, tags, facets, and insights) as JSON for the
// requested filter parameters.
func (s *Server) handleSummaryAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	filter, _, err := parseFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, err := s.buildPayload(r.Context(), filter, r.URL.Query())
	if err != nil {
		s.log.Error("summary api failed", "error", err)
		http.Error(w, "could not build summary", http.StatusInternalServerError)
		return
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(data.PayloadJSON), &out); err != nil {
		http.Error(w, "could not encode summary", http.StatusInternalServerError)
		return
	}
	out["burn"] = data.Burn
	out["facets"] = data.Facets
	out["insights"] = data.Insights
	out["scan"] = data.Scan
	writeJSON(w, out)
}

// handleInsightsAPI serves GET /api/insights, returning ordered insights for
// the requested filter parameters.
func (s *Server) handleInsightsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	filter, _, err := parseFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, err := s.buildPayload(r.Context(), filter, r.URL.Query())
	if err != nil {
		s.log.Error("insights api failed", "error", err)
		http.Error(w, "could not build insights", http.StatusInternalServerError)
		return
	}
	insights := data.Insights
	if insights == nil {
		insights = []Insight{}
	}
	writeJSON(w, insights)
}

// handleDigestAPI serves GET /api/digest, returning the usage digest markdown
// (as JSON by default, or text/markdown when ?format=markdown).
func (s *Server) handleDigestAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	filter, _, err := parseFilter(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.applyTagFilter(r.Context(), &filter, q)
	filter.DateFrom, filter.DateTo = nil, nil

	days := 7
	if raw := strings.TrimSpace(q.Get("days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 3650 {
			http.Error(w, "days must be an integer between 1 and 3650", http.StatusBadRequest)
			return
		}
		days = n
	}

	md, err := BuildDigest(r.Context(), s.store, DigestOptions{
		Filter:        filter,
		Days:          days,
		DailyBudget:   s.DailyBudget,
		MonthlyBudget: s.MonthlyBudget,
		Now:           s.Generated(),
	})
	if err != nil {
		s.log.Error("digest api failed", "error", err)
		http.Error(w, "could not build digest", http.StatusInternalServerError)
		return
	}
	if strings.EqualFold(q.Get("format"), "markdown") || strings.EqualFold(q.Get("format"), "md") {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, md)
		return
	}
	writeJSON(w, map[string]any{
		"days":     days,
		"markdown": md,
	})
}

// handleStatsAPI serves GET /api/stats, returning overall database totals and
// per-source scan status (mirroring `dashd stats`).
func (s *Server) handleStatsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	totals, err := s.store.Totals(ctx, store.Filter{})
	if err != nil {
		http.Error(w, "could not read totals", http.StatusInternalServerError)
		return
	}
	scans, err := s.store.ScanStatuses()
	if err != nil {
		http.Error(w, "could not read scan status", http.StatusInternalServerError)
		return
	}
	machines, err := s.store.Machines(ctx)
	if err != nil {
		http.Error(w, "could not read machines", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"sessions":      totals.Sessions,
		"calls":         totals.Calls,
		"total_tokens":  totals.TotalTokens,
		"input_tokens":  totals.InputTokens,
		"output_tokens": totals.OutputTokens,
		"cost_usd":      totals.Cost,
		"unpriced":      totals.UnpricedCalls,
		"machines":      machines,
		"scans":         scans,
	})
}

// handleSchemaAPI serves GET /api/schema, returning all SQLite tables, views,
// and columns so LLM clients and scripts can discover the schema before
// querying /api/query.
func (s *Server) handleSchemaAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tables, err := s.store.Schema(r.Context())
	if err != nil {
		s.log.Error("schema api failed", "error", err)
		http.Error(w, "could not read schema", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"tables": tables})
}

type queryRequest struct {
	SQL   string `json:"sql"`
	Limit int    `json:"limit"`
}

// handleQueryAPI serves POST /api/query (JSON body {"sql": "...", "limit": 500})
// and GET /api/query?sql=...&limit=... to execute a single read-only SQL query
// against the store.
func (s *Server) handleQueryAPI(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	switch r.Method {
	case http.MethodGet:
		req.SQL = r.URL.Query().Get("sql")
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				http.Error(w, "limit must be a non-negative integer", http.StatusBadRequest)
				return
			}
			req.Limit = n
		}
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxQueryBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	res, err := s.store.QueryReadOnly(r.Context(), req.SQL, req.Limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, res)
}
