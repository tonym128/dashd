package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/store"
)

// CSVSafe defuses spreadsheet formula injection: a cell that starts with
// = + - @ (or a tab/CR) is run as a formula by Excel and Sheets, and project
// paths, titles and model ids come from logs.
func CSVSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

var callCSVHeader = []string{"session_uid", "call_key", "agent", "project", "machine", "model", "time", "day",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
	"total_tokens", "llm_seconds", "cost_usd", "priced", "ingested_at"}

// WriteCallsCSV streams every call matching f as CSV.
func WriteCallsCSV(ctx context.Context, st *store.Store, f store.Filter, w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(callCSVHeader); err != nil {
		return err
	}
	err := st.ExportCalls(ctx, f, func(c store.ExportCall) error {
		return cw.Write([]string{CSVSafe(c.SessionUID), CSVSafe(c.CallKey), c.Agent, CSVSafe(c.Project), CSVSafe(c.Machine),
			CSVSafe(c.Model), rfc3339(c.TS), c.Day,
			i64(c.InputTokens), i64(c.OutputTokens), i64(c.CacheReadTokens), i64(c.CacheWriteTokens),
			i64(c.ReasoningTokens), i64(c.TotalTokens), f64(c.LLMSeconds, 3), f64(c.CostUSD, 6),
			strconv.FormatBool(c.Priced), rfc3339(c.IngestedAt)})
	})
	if err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// WriteCallsJSON streams every call matching f as a JSON array.
func WriteCallsJSON(ctx context.Context, st *store.Store, f store.Filter, w io.Writer) error {
	if _, err := io.WriteString(w, "[\n"); err != nil {
		return err
	}
	first := true
	err := st.ExportCalls(ctx, f, func(c store.ExportCall) error {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if !first {
			if _, err := io.WriteString(w, ",\n"); err != nil {
				return err
			}
		}
		first = false
		_, err = w.Write(b)
		return err
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n]\n")
	return err
}

func rfc3339(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format(time.RFC3339)
}
func i64(v int64) string          { return strconv.FormatInt(v, 10) }
func f64(v float64, p int) string { return strconv.FormatFloat(v, 'f', p, 64) }

// handleExport serves server-side exports of any table, respecting the
// current filter and not capped at what the page rendered.
//
//	GET /api/export?kind=calls|sessions|projects|models|machines|daily|tools|tags&format=csv|json&<filter>
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
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
	kind := q.Get("kind")
	format := q.Get("format")
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		http.Error(w, "format must be csv or json", http.StatusBadRequest)
		return
	}
	// A full export of a large history takes longer than the server-wide
	// write timeout allows a page.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))

	header, rows, err := s.exportTable(r.Context(), kind, filter)
	if kind == "calls" {
		err = nil
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := fmt.Sprintf("dashd-%s-%s.%s", kind, time.Now().Format("20060102"), format)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}

	if kind == "calls" {
		if format == "csv" {
			err = WriteCallsCSV(r.Context(), s.store, filter, w)
		} else {
			err = WriteCallsJSON(r.Context(), s.store, filter, w)
		}
		if err != nil {
			s.log.Warn("export failed mid-stream", "kind", kind, "error", err)
		}
		return
	}
	if format == "csv" {
		cw := csv.NewWriter(w)
		_ = cw.Write(header)
		for _, row := range rows {
			cells := make([]string, len(row))
			for i, v := range row {
				cells[i] = CSVSafe(fmt.Sprint(v))
			}
			_ = cw.Write(cells)
		}
		cw.Flush()
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		m := make(map[string]any, len(header))
		for i, h := range header {
			m[h] = row[i]
		}
		out = append(out, m)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func (s *Server) exportTable(ctx context.Context, kind string, f store.Filter) ([]string, [][]any, error) {
	var rows [][]any
	switch kind {
	case "calls":
		return nil, nil, nil
	case "sessions":
		header := []string{"uid", "agent", "project", "machine", "title", "start", "end", "wall_seconds", "calls",
			"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
			"total_tokens", "llm_seconds", "tool_seconds", "tool_calls", "tool_errors", "cost_usd", "orphaned"}
		for offset := 0; ; offset += 1000 {
			page, _, err := s.store.SessionsPage(ctx, f, store.SessionQuery{Limit: 1000, Offset: offset})
			if err != nil {
				return nil, nil, err
			}
			for _, r := range page {
				rows = append(rows, []any{r.UID, r.Agent, r.Project, r.Machine, r.Title, rfc3339(r.FirstTS), rfc3339(r.LastTS),
					r.WallSeconds, r.Calls, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens,
					r.ReasoningTokens, r.TotalTokens, r.LLMSeconds, r.ToolSeconds, r.ToolCalls, r.ToolErrors,
					f64(r.Cost, 6), r.Orphaned})
			}
			if len(page) < 1000 {
				break
			}
		}
		return header, rows, nil
	case "projects":
		ps, err := s.store.Projects(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range projectsJSON(ps, nil, nil) {
			rows = append(rows, []any{p["name"], strings.Join(p["agents"].([]string), " "), p["sessions"], p["messages"],
				p["tokens"], p["llm_time"], p["tool_time"], f64(p["cost"].(float64), 6), f64(p["per_session"].(float64), 6),
				f64(p["per_call"].(float64), 6), rfc3339(p["last_activity"].(int64))})
		}
		return []string{"project", "agents", "sessions", "calls", "tokens", "llm_seconds", "tool_seconds", "cost_usd",
			"cost_per_session", "cost_per_call", "last_activity"}, rows, nil
	case "models":
		ms, err := s.store.Models(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		for _, m := range ms {
			rows = append(rows, []any{m.Model, m.Calls, m.TotalTokens, m.InputTokens, m.OutputTokens, m.CacheReadTokens,
				m.CacheWriteTokens, m.ReasoningTokens, m.LLMSeconds, f64(m.Cost, 6), f64(ratio(m.Cost, float64(m.Calls)), 6),
				f64(ratio(m.Cost*1000, float64(m.OutputTokens+m.ReasoningTokens)), 6), m.Unpriced})
		}
		return []string{"model", "calls", "total_tokens", "input_tokens", "output_tokens", "cache_read_tokens",
			"cache_write_tokens", "reasoning_tokens", "llm_seconds", "cost_usd", "cost_per_call",
			"cost_per_1k_output", "unpriced_calls"}, rows, nil
	case "machines":
		ms, err := s.store.MachinesBreakdown(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		for _, m := range ms {
			rows = append(rows, []any{m.Machine, m.Sessions, m.Calls, m.TotalTokens, m.LLMSeconds, f64(m.Cost, 6),
				f64(ratio(m.Cost, float64(m.Sessions)), 6), rfc3339(m.LastTS)})
		}
		return []string{"machine", "sessions", "calls", "total_tokens", "llm_seconds", "cost_usd", "cost_per_session",
			"last_activity"}, rows, nil
	case "daily":
		days, err := s.store.Daily(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		for _, d := range days {
			models := make([]string, 0, len(d.Models))
			for m := range d.Models {
				models = append(models, m)
			}
			sort.Strings(models)
			for _, m := range models {
				rows = append(rows, []any{d.Day, m, f64(d.Models[m], 6)})
			}
		}
		return []string{"day", "model", "cost_usd"}, rows, nil
	case "tools":
		ts, err := s.store.Tools(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		for _, t := range ts {
			rows = append(rows, []any{t.Tool, t.Calls, t.Seconds, t.Errors, f64(t.Cost, 6)})
		}
		return []string{"tool", "calls", "seconds", "errors", "associated_cost_usd"}, rows, nil
	case "tags":
		ps, err := s.store.Projects(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		var total float64
		for _, p := range ps {
			total += p.Cost
		}
		for _, t := range tagsJSON(s.Tags, ps, total) {
			rows = append(rows, []any{t["tag"], t["projects"], t["sessions"], t["calls"], f64(t["cost"].(float64), 6),
				f64(t["pct"].(float64), 2)})
		}
		return []string{"tag", "projects", "sessions", "calls", "cost_usd", "share_pct"}, rows, nil
	}
	return nil, nil, fmt.Errorf("unknown export kind %q", kind)
}
