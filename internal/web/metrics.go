package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tonym128/dashd/internal/store"
)

// handleMetrics exposes Prometheus text-format metrics for monitoring.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	var totals store.Totals
	var machines []store.MachineStatus
	err := s.store.ReadTx(ctx, func(st *store.Store) error {
		var err error
		if totals, err = st.Totals(ctx, store.Filter{}); err != nil {
			return err
		}
		machines, err = st.MachineStatuses(ctx)
		return err
	})
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# HELP dashd_calls_total Total stored LLM calls.\n")
	fmt.Fprintf(&b, "# TYPE dashd_calls_total counter\n")
	fmt.Fprintf(&b, "dashd_calls_total %d\n", totals.Calls)
	fmt.Fprintf(&b, "# HELP dashd_sessions_total Total stored sessions.\n")
	fmt.Fprintf(&b, "# TYPE dashd_sessions_total gauge\n")
	fmt.Fprintf(&b, "dashd_sessions_total %d\n", totals.Sessions)
	fmt.Fprintf(&b, "# HELP dashd_cost_usd_total Total computed cost in USD.\n")
	fmt.Fprintf(&b, "# TYPE dashd_cost_usd_total counter\n")
	fmt.Fprintf(&b, "dashd_cost_usd_total %.6f\n", totals.Cost)
	fmt.Fprintf(&b, "# HELP dashd_tokens_total Total tokens across all calls.\n")
	fmt.Fprintf(&b, "# TYPE dashd_tokens_total counter\n")
	fmt.Fprintf(&b, "dashd_tokens_total %d\n", totals.TotalTokens)
	fmt.Fprintf(&b, "# HELP dashd_unpriced_calls_total Total calls with no known model rate.\n")
	fmt.Fprintf(&b, "# TYPE dashd_unpriced_calls_total gauge\n")
	fmt.Fprintf(&b, "dashd_unpriced_calls_total %d\n", totals.UnpricedCalls)

	if len(machines) > 0 {
		fmt.Fprintf(&b, "# HELP dashd_machine_last_seen_seconds Unix timestamp of last push from each machine.\n")
		fmt.Fprintf(&b, "# TYPE dashd_machine_last_seen_seconds gauge\n")
		for _, m := range machines {
			esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(m.Machine)
			fmt.Fprintf(&b, "dashd_machine_last_seen_seconds{machine=%q} %d\n", esc, m.LastSeenAt)
		}
	}
	_, _ = w.Write([]byte(b.String()))
}

// handleSessionsAPI returns a paginated, filtered JSON list of sessions.
func (s *Server) handleSessionsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	filter, _, err := parseFilter(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	offset := 0
	if raw := q.Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}
	search := strings.ToLower(strings.TrimSpace(q.Get("q")))

	rows, err := s.store.Sessions(r.Context(), filter, 5000)
	if err != nil {
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}

	if search != "" {
		filtered := make([]store.SessionRow, 0, len(rows))
		for _, row := range rows {
			if strings.Contains(strings.ToLower(row.Project), search) ||
				strings.Contains(strings.ToLower(row.Path), search) ||
				strings.Contains(strings.ToLower(row.Agent), search) ||
				strings.Contains(strings.ToLower(row.Title), search) ||
				strings.Contains(strings.ToLower(row.Machine), search) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}

	total := len(rows)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	writeJSON(w, map[string]any{
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"sessions": sessionsJSON(rows[offset:end]),
	})
}
