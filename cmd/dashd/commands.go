package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/store"
	"github.com/tonym128/dashd/internal/web"
)

// filterFromOptions builds a store filter from the CLI filter flags.
func filterFromOptions(o options) (store.Filter, error) {
	f := store.Filter{
		Projects: splitList(o.project),
		Agents:   splitList(o.agent),
		Models:   splitList(o.modelFilter),
		Machines: splitList(o.onlyMachine),
	}
	if o.since != "" {
		t, err := time.ParseInLocation("2006-01-02", o.since, time.Local)
		if err != nil {
			return f, fmt.Errorf("-since %q is not YYYY-MM-DD", o.since)
		}
		f.DateFrom = &t
	}
	if o.until != "" {
		t, err := time.ParseInLocation("2006-01-02", o.until, time.Local)
		if err != nil {
			return f, fmt.Errorf("-until %q is not YYYY-MM-DD", o.until)
		}
		end := t.AddDate(0, 0, 1).Add(-time.Second)
		f.DateTo = &end
	}
	return f, nil
}

func runInsights(ctx context.Context, o options, st *store.Store, out io.Writer) error {
	f, err := filterFromOptions(o)
	if err != nil {
		return err
	}
	insights, err := web.BuildInsightsFromStore(ctx, st, f, time.Now())
	if err != nil {
		return err
	}
	if o.jsonOut {
		if insights == nil {
			insights = []web.Insight{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(insights)
	}
	if len(insights) == 0 {
		fmt.Fprintln(out, "No insights yet.")
		return nil
	}
	for _, in := range insights {
		fmt.Fprintf(out, "[%-4s] %-17s %s\n       %s\n", strings.ToUpper(in.Severity), in.Category, in.Headline, in.Detail)
	}
	return nil
}

func runExport(ctx context.Context, o options, st *store.Store, stdout io.Writer) error {
	f, err := filterFromOptions(o)
	if err != nil {
		return err
	}
	w := stdout
	if o.out != "" && o.out != "-" {
		file, err := os.OpenFile(o.out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		w = file
	}
	switch o.format {
	case "csv":
		return writeCallsCSV(ctx, st, f, w)
	case "json":
		return writeCallsJSON(ctx, st, f, w)
	}
	return fmt.Errorf("-format %q: want csv or json", o.format)
}

// CallCSVHeader is the column order of a call export.
var callCSVHeader = []string{"session_uid", "call_key", "agent", "project", "machine", "model", "time", "day",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
	"total_tokens", "llm_seconds", "cost_usd", "priced", "ingested_at"}

func writeCallsCSV(ctx context.Context, st *store.Store, f store.Filter, w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(callCSVHeader); err != nil {
		return err
	}
	err := st.ExportCalls(ctx, f, func(c store.ExportCall) error {
		ts := ""
		if c.TS > 0 {
			ts = time.Unix(c.TS, 0).Format(time.RFC3339)
		}
		ing := ""
		if c.IngestedAt > 0 {
			ing = time.Unix(c.IngestedAt, 0).Format(time.RFC3339)
		}
		return cw.Write([]string{c.SessionUID, c.CallKey, c.Agent, csvSafe(c.Project), csvSafe(c.Machine), csvSafe(c.Model), ts, c.Day,
			strconv.FormatInt(c.InputTokens, 10), strconv.FormatInt(c.OutputTokens, 10),
			strconv.FormatInt(c.CacheReadTokens, 10), strconv.FormatInt(c.CacheWriteTokens, 10),
			strconv.FormatInt(c.ReasoningTokens, 10), strconv.FormatInt(c.TotalTokens, 10),
			strconv.FormatFloat(c.LLMSeconds, 'f', 3, 64), strconv.FormatFloat(c.CostUSD, 'f', 6, 64),
			strconv.FormatBool(c.Priced), ing})
	})
	if err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// csvSafe defuses spreadsheet formula injection: a cell that starts with = + -
// @ is run as a formula by Excel and Sheets, and project paths and model ids
// come from logs.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func writeCallsJSON(ctx context.Context, st *store.Store, f store.Filter, w io.Writer) error {
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

func runDigest(ctx context.Context, o options, st *store.Store, out io.Writer) error {
	f, err := filterFromOptions(o)
	if err != nil {
		return err
	}
	days := 7
	if o.digestEvery > 0 {
		days = max(1, int(o.digestEvery.Hours()/24))
	}
	f.DateFrom, f.DateTo = nil, nil
	md, err := web.BuildDigest(ctx, st, web.DigestOptions{Filter: f, Days: days,
		DailyBudget: o.dailyBudget, MonthlyBudget: o.monthlyBudget})
	if err != nil {
		return err
	}
	if o.webhookURL != "" {
		if err := web.PostWebhook(ctx, &http.Client{Timeout: 20 * time.Second}, o.webhookURL, md); err != nil {
			return err
		}
		fmt.Fprintln(out, "digest sent")
		return nil
	}
	_, err = io.WriteString(out, md)
	return err
}

// pushStatus is what `dashd status` reports.
type pushStatus struct {
	StateDB       string `json:"state_db"`
	Remote        string `json:"remote"`
	Machine       string `json:"machine"`
	ServerVersion string `json:"server_version,omitempty"`
	InstanceID    string `json:"instance_id,omitempty"`
	LastOK        string `json:"last_ok,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastErrorAt   string `json:"last_error_at,omitempty"`
	TrackedFiles  int    `json:"tracked_files"`
	ServerHealthy *bool  `json:"server_healthy,omitempty"`
}

func runStatus(ctx context.Context, o options, st *store.Store, out io.Writer) error {
	s := pushStatus{StateDB: o.db, Machine: o.machine}
	meta := func(k string) string { v, _ := st.Meta(k); return v }
	s.Remote = meta(metaRemote)
	if s.Remote == "" {
		s.Remote = o.remote
	}
	s.ServerVersion = meta(metaServerVersion)
	s.InstanceID = meta(metaInstanceID)
	s.LastOK = unixMeta(meta(metaLastOKAt))
	s.LastError = meta(metaLastError)
	if s.LastError != "" {
		s.LastErrorAt = unixMeta(meta(metaLastErrorAt))
	}
	s.TrackedFiles, _ = st.CountScanStates()
	if s.Remote != "" {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimRight(s.Remote, "/")+"/healthz", nil)
		if err == nil {
			healthy := false
			if resp, err := http.DefaultClient.Do(req); err == nil {
				healthy = resp.StatusCode == http.StatusOK
				resp.Body.Close()
			}
			s.ServerHealthy = &healthy
		}
	}
	if o.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	row := func(k, v string) {
		if v == "" {
			v = "-"
		}
		fmt.Fprintf(out, "%-15s %s\n", k, v)
	}
	row("state db", s.StateDB)
	row("machine", s.Machine)
	row("remote", s.Remote)
	if s.ServerHealthy != nil {
		row("server", map[bool]string{true: "reachable", false: "UNREACHABLE"}[*s.ServerHealthy])
	}
	row("server version", s.ServerVersion)
	row("last success", s.LastOK)
	if s.LastError != "" {
		row("last error", s.LastErrorAt+"  "+s.LastError)
	} else {
		row("last error", "none")
	}
	row("tracked files", strconv.Itoa(s.TrackedFiles))
	return nil
}

func unixMeta(v string) string {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n == 0 {
		return ""
	}
	t := time.Unix(n, 0)
	return fmt.Sprintf("%s (%s ago)", t.Format("2006-01-02 15:04:05"), time.Since(t).Round(time.Second))
}
