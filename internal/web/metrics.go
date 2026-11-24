package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
)

// handleMetrics exposes Prometheus text-format metrics for monitoring.
//
// Stored totals are gauges, not counters: a reprice or a prune can lower them,
// and a counter that goes down breaks rate() and increase().
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	var totals store.Totals
	var machines []store.MachineStatus
	var byMachine, byAgent map[string]float64
	var statuses []model.ScanStatus
	err := s.store.ReadTx(ctx, func(st *store.Store) error {
		var err error
		if totals, err = st.Totals(ctx, store.Filter{}); err != nil {
			return err
		}
		if machines, err = st.MachineStatuses(ctx); err != nil {
			return err
		}
		if byMachine, err = st.CostBy(ctx, "machine"); err != nil {
			return err
		}
		if byAgent, err = st.CostBy(ctx, "agent"); err != nil {
			return err
		}
		statuses, err = st.ScanStatuses()
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
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	counter := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	}
	gauge("dashd_calls", "Stored LLM calls.")
	fmt.Fprintf(&b, "dashd_calls %d\n", totals.Calls)
	gauge("dashd_sessions", "Stored sessions.")
	fmt.Fprintf(&b, "dashd_sessions %d\n", totals.Sessions)
	gauge("dashd_cost_usd", "Stored cost in USD (can fall after a reprice or prune).")
	fmt.Fprintf(&b, "dashd_cost_usd %.6f\n", totals.Cost)
	gauge("dashd_tokens", "Stored tokens across all calls.")
	fmt.Fprintf(&b, "dashd_tokens %d\n", totals.TotalTokens)
	gauge("dashd_unpriced_calls", "Stored calls with no known model rate.")
	fmt.Fprintf(&b, "dashd_unpriced_calls %d\n", totals.UnpricedCalls)

	gauge("dashd_cost_usd_by_machine", "Stored cost in USD per machine.")
	for _, k := range sortedKeys(byMachine) {
		fmt.Fprintf(&b, "dashd_cost_usd_by_machine{machine=\"%s\"} %.6f\n", promEscape(k), byMachine[k])
	}
	gauge("dashd_cost_usd_by_agent", "Stored cost in USD per agent.")
	for _, k := range sortedKeys(byAgent) {
		fmt.Fprintf(&b, "dashd_cost_usd_by_agent{agent=\"%s\"} %.6f\n", promEscape(k), byAgent[k])
	}

	if len(machines) > 0 {
		gauge("dashd_machine_last_seen_seconds", "Unix timestamp of the last push from each machine.")
		for _, m := range machines {
			fmt.Fprintf(&b, "dashd_machine_last_seen_seconds{machine=\"%s\"} %d\n", promEscape(m.Machine), m.LastSeenAt)
		}
		gauge("dashd_machine_failing", "1 when the machine's push agent last reported an error.")
		for _, m := range machines {
			v := 0
			if m.LastError != "" {
				v = 1
			}
			fmt.Fprintf(&b, "dashd_machine_failing{machine=\"%s\"} %d\n", promEscape(m.Machine), v)
		}
	}

	counter("dashd_ingest_requests_total", "Successful POST /api/ingest requests since start.")
	fmt.Fprintf(&b, "dashd_ingest_requests_total %d\n", s.ingestRequests.Load())
	counter("dashd_ingest_calls_total", "Calls received through /api/ingest since start.")
	fmt.Fprintf(&b, "dashd_ingest_calls_total %d\n", s.ingestCalls.Load())

	if len(statuses) > 0 {
		gauge("dashd_scan_last_seconds", "Unix timestamp of the last scan of each source.")
		gauge("dashd_scan_failing", "1 when the last scan of a source failed.")
		for _, st := range statuses {
			fail := 0
			if st.Error != "" {
				fail = 1
			}
			fmt.Fprintf(&b, "dashd_scan_last_seconds{agent=\"%s\"} %d\n", promEscape(st.Agent), st.LastScanAt.Unix())
			fmt.Fprintf(&b, "dashd_scan_failing{agent=\"%s\"} %d\n", promEscape(st.Agent), fail)
		}
	}
	if s.ScanDuration != nil {
		gauge("dashd_scan_duration_seconds", "Duration of the most recent scan pass.")
		fmt.Fprintf(&b, "dashd_scan_duration_seconds %.3f\n", s.ScanDuration().Seconds())
	}
	_, _ = w.Write([]byte(b.String()))
}

func promEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// handleSessionsAPI returns a paginated, filtered, sorted JSON list of
// sessions. Paging, sorting and search all happen in SQL, so there is no cap.
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
	s.applyTagFilter(r.Context(), &filter, q)
	sq := store.SessionQuery{Search: q.Get("q"), Sort: q.Get("sort"), Asc: q.Get("asc") == "1" || q.Get("asc") == "true"}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		sq.Limit = n
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil {
		sq.Offset = n
	}
	rows, total, err := s.store.SessionsPage(r.Context(), filter, sq)
	if err != nil {
		s.log.Error("sessions api failed", "error", err)
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}
	uids := make([]string, len(rows))
	for i, row := range rows {
		uids[i] = row.UID
	}
	models, err := s.store.SessionModels(r.Context(), uids)
	if err != nil {
		s.log.Warn("session models failed", "error", err)
	}
	limit := sq.Limit
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	writeJSON(w, map[string]any{
		"total":    total,
		"limit":    limit,
		"offset":   max(sq.Offset, 0),
		"sessions": sessionsJSON(rows, models),
	})
}

// applyTagFilter expands ?tag= into the projects it covers.
func (s *Server) applyTagFilter(ctx context.Context, f *store.Filter, q url.Values) {
	tags := q["tag"]
	if len(tags) == 0 || len(s.Tags) == 0 {
		return
	}
	_, _, all, _, _, err := s.store.Facets(ctx)
	if err != nil {
		return
	}
	matched := s.Tags.ProjectsFor(tags, all)
	if len(matched) == 0 {
		matched = []string{"\x00no-project-matches"}
	}
	f.Projects = append(f.Projects, matched...)
}
