package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tonym128/dashd/internal/store"
	"github.com/tonym128/dashd/internal/web"
)

// readTokenForRemote picks the bearer token for read-only remote CLI and MCP
// calls: -read-token first, falling back to -auth-token.
func readTokenForRemote(o options) string {
	if o.readToken != "" {
		return o.readToken
	}
	return o.authToken
}

type remoteClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newRemoteClient(baseURL, token string) *remoteClient {
	return &remoteClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *remoteClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("remote %s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	return data, nil
}

func filterQueryParams(o options) url.Values {
	q := url.Values{}
	if o.since != "" {
		q.Set("from", o.since)
	}
	if o.until != "" {
		q.Set("to", o.until)
	}
	if o.project != "" {
		q.Set("project", o.project)
	}
	if o.agent != "" {
		q.Set("agent", o.agent)
	}
	if o.modelFilter != "" {
		q.Set("model", o.modelFilter)
	}
	if o.onlyMachine != "" {
		q.Set("machine", o.onlyMachine)
	}
	return q
}

func withQueryString(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func runRemoteStats(ctx context.Context, o options, out io.Writer) error {
	c := newRemoteClient(o.remote, readTokenForRemote(o))
	data, err := c.do(ctx, http.MethodGet, "/api/stats", nil)
	if err != nil {
		return err
	}
	if o.jsonOut {
		_, err = out.Write(data)
		return err
	}
	var stats struct {
		Sessions    int      `json:"sessions"`
		Calls       int      `json:"calls"`
		TotalTokens int64    `json:"total_tokens"`
		CostUSD     float64  `json:"cost_usd"`
		Unpriced    int      `json:"unpriced"`
		Machines    []string `json:"machines"`
	}
	if err := json.Unmarshal(data, &stats); err != nil {
		return err
	}
	fmt.Fprintf(out, "remote:   %s\n", o.remote)
	fmt.Fprintf(out, "sessions: %d\n", stats.Sessions)
	fmt.Fprintf(out, "calls:    %d (unpriced: %d)\n", stats.Calls, stats.Unpriced)
	fmt.Fprintf(out, "tokens:   %d\n", stats.TotalTokens)
	fmt.Fprintf(out, "cost:     $%.4f\n", stats.CostUSD)
	if len(stats.Machines) > 0 {
		fmt.Fprintf(out, "machines: %s\n", strings.Join(stats.Machines, ", "))
	}
	return nil
}

func runRemoteInsights(ctx context.Context, o options, out io.Writer) error {
	c := newRemoteClient(o.remote, readTokenForRemote(o))
	path := withQueryString("/api/insights", filterQueryParams(o))
	data, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if o.jsonOut {
		_, err = out.Write(data)
		return err
	}
	var insights []web.Insight
	if err := json.Unmarshal(data, &insights); err != nil {
		return err
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

func runRemoteDigest(ctx context.Context, o options, out io.Writer) error {
	c := newRemoteClient(o.remote, readTokenForRemote(o))
	q := filterQueryParams(o)
	q.Del("from")
	q.Del("to")
	days := 7
	if o.digestEvery > 0 {
		days = max(1, int(o.digestEvery.Hours()/24))
	}
	q.Set("days", strconv.Itoa(days))
	q.Set("format", "markdown")
	data, err := c.do(ctx, http.MethodGet, withQueryString("/api/digest", q), nil)
	if err != nil {
		return err
	}
	md := string(data)
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

func runRemoteExport(ctx context.Context, o options, stdout io.Writer) error {
	if o.format != "csv" && o.format != "json" {
		return fmt.Errorf("-format %q: want csv or json", o.format)
	}
	c := newRemoteClient(o.remote, readTokenForRemote(o))
	q := filterQueryParams(o)
	q.Set("format", o.format)
	q.Set("kind", "calls")
	data, err := c.do(ctx, http.MethodGet, withQueryString("/api/export", q), nil)
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
	_, err = w.Write(data)
	return err
}

func runQuery(ctx context.Context, o options, st *store.Store, out io.Writer) error {
	if o.showSchema {
		var tables []store.TableSchema
		if o.remote != "" {
			c := newRemoteClient(o.remote, readTokenForRemote(o))
			data, err := c.do(ctx, http.MethodGet, "/api/schema", nil)
			if err != nil {
				return err
			}
			var resp struct {
				Tables []store.TableSchema `json:"tables"`
			}
			if err := json.Unmarshal(data, &resp); err != nil {
				return err
			}
			tables = resp.Tables
		} else {
			var err error
			tables, err = st.Schema(ctx)
			if err != nil {
				return err
			}
		}
		if o.jsonOut {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{"tables": tables})
		}
		for _, tbl := range tables {
			fmt.Fprintf(out, "%s (%s)\n", tbl.Name, tbl.Type)
			for _, col := range tbl.Columns {
				fmt.Fprintf(out, "  %-22s %s\n", col.Name, col.Type)
			}
		}
		return nil
	}

	if strings.TrimSpace(o.sqlQuery) == "" {
		return fmt.Errorf("-sql \"SELECT ...\" or -schema is required for query")
	}

	var res store.QueryResult
	if o.remote != "" {
		c := newRemoteClient(o.remote, readTokenForRemote(o))
		data, err := c.do(ctx, http.MethodPost, "/api/query", map[string]any{
			"sql":   o.sqlQuery,
			"limit": o.queryLimit,
		})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &res); err != nil {
			return err
		}
	} else {
		var err error
		res, err = st.QueryReadOnly(ctx, o.sqlQuery, o.queryLimit)
		if err != nil {
			return err
		}
	}

	if o.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(res.Columns, "\t"))
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				cells[i] = ""
			} else {
				cells[i] = fmt.Sprint(v)
			}
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if res.Truncated {
		fmt.Fprintf(out, "(truncated at %d rows; raise -limit to see more)\n", res.RowCount)
	}
	return nil
}
