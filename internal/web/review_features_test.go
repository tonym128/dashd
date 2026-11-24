package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
)

func TestSessionPageShowsBreakdownsAndResumeCommand(t *testing.T) {
	srv, st := newTestServer(t)
	now := time.Now()
	uid := "beta::abc-123"
	if err := st.ReplaceSession(model.SessionWrite{
		Session: model.Session{UID: uid, Agent: "claude", Project: "/p/x", Machine: "beta"},
		Calls: []model.Call{
			{SessionUID: uid, CallKey: "1", Agent: "claude", Model: "m-a", Time: now.Add(-time.Hour), CostUSD: 1, Priced: true, TotalTokens: 10},
			{SessionUID: uid, CallKey: "2", Agent: "claude", Model: "m-b", Time: now, CostUSD: 3, Priced: true, TotalTokens: 10},
		},
		ToolCalls: []model.ToolCall{{SessionUID: uid, CallKey: "t", Agent: "claude", Tool: "Bash", Time: now, Seconds: 2, IsError: true}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.RecomputeSession(uid)
	body := get(t, srv, "/session?uid="+uid).Body.String()
	for _, want := range []string{"Cost by model", "m-b", "75.0%", "Bash", "Cost over the session",
		"--resume &#39;abc-123&#39;", "/assets/session.js", "Arrived"} {
		if !strings.Contains(body, want) {
			t.Errorf("session page is missing %q", want)
		}
	}
	if strings.Contains(body, "beta::abc-123&#39;") {
		t.Error("resume command carries the server's machine namespace")
	}
}

func TestSessionsAPISortsAndPagesInSQL(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	var page struct {
		Total    int              `json:"total"`
		Sessions []map[string]any `json:"sessions"`
	}
	_ = json.Unmarshal(get(t, srv, "/api/sessions?sort=cost&limit=1").Body.Bytes(), &page)
	if page.Total != 3 || len(page.Sessions) != 1 || page.Sessions[0]["cost"].(float64) != 3 {
		t.Fatalf("page = %+v", page)
	}
	_ = json.Unmarshal(get(t, srv, "/api/sessions?q=two").Body.Bytes(), &page)
	if page.Total != 1 {
		t.Fatalf("search total = %d", page.Total)
	}
}

func TestExportEndpoint(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	for _, kind := range []string{"calls", "sessions", "projects", "models", "machines", "daily", "tools", "tags"} {
		rec := get(t, srv, "/api/export?kind="+kind)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") ||
			!strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("export %s = %d %q", kind, rec.Code, rec.Header().Get("Content-Type"))
			continue
		}
		rows, err := csv.NewReader(rec.Body).ReadAll()
		if err != nil || len(rows) < 1 {
			t.Errorf("export %s: %v", kind, err)
		}
	}
	rec := get(t, srv, "/api/export?kind=calls&format=json&project=/p/two")
	var calls []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &calls); err != nil || len(calls) != 1 {
		t.Fatalf("filtered json export = %d calls, %v", len(calls), err)
	}
	if get(t, srv, "/api/export?kind=bogus").Code != http.StatusBadRequest {
		t.Error("unknown export kind accepted")
	}
}

func TestMetricsAreGaugesWithBreakdowns(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	body := get(t, srv, "/metrics").Body.String()
	for _, want := range []string{"# TYPE dashd_cost_usd gauge", `dashd_cost_usd_by_agent{agent="claude"} 3.000000`,
		"# TYPE dashd_ingest_requests_total counter"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(body, "dashd_cost_usd_total counter") {
		t.Error("cost is still declared a counter")
	}
}

func TestBurnCardsIgnoreTheDateRange(t *testing.T) {
	srv, st := newTestServer(t)
	srv.Generated = time.Now
	now := time.Now()
	seed(t, st, now)
	today := now.Format("2006-01-02")
	unfiltered := get(t, srv, "/").Body.String()
	filtered := get(t, srv, "/?date_from=2000-01-01&date_to=2000-01-02").Body.String()
	pick := func(body string) string {
		i := strings.Index(body, `class="burn-value"`)
		return body[i : i+40]
	}
	if pick(unfiltered) != pick(filtered) {
		t.Errorf("Today's burn changed under an old date range: %q vs %q", pick(unfiltered), pick(filtered))
	}
	if !strings.Contains(filtered, "Burn cards ignore the date range") {
		t.Error("no note that burn ignores the date range")
	}
	_ = today
}

func TestForecastAndCacheSavings(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	burn := []burnWindow{{Label: "Last 7 days", Cost: 70, Days: 7}, {Label: "Month to date", Cost: 50, Days: 10}}
	f := monthForecast(burn, now)
	if !f.OK || f.DaysLeft != 20 || f.Low != 150 || f.High != 250 || f.Projected != 200 {
		t.Fatalf("forecast = %+v", f)
	}
	price := func(m string, in, out, cr, cw, cw1h, _ int64) (float64, bool) {
		return float64(in)*3e-6 + float64(cr)*0.3e-6, true
	}
	got := cacheSavings([]store.ModelStat{{Model: "x", CacheReadTokens: 1_000_000}}, price)
	if got < 2.69 || got > 2.71 {
		t.Fatalf("savings = %v, want 2.70", got)
	}
	ins := BuildInsightsFull(InsightInputs{Totals: store.Totals{Calls: 1, Cost: 1, InputTokens: 1, CacheReadTokens: 10},
		Burn: burn, Forecast: f, MonthlyBudget: 100, CacheSavings: 2.7, Now: now})
	ids := map[string]bool{}
	for _, in := range ins {
		ids[in.ID] = true
	}
	if !ids["monthly-forecast-over"] || !ids["cache-savings"] {
		t.Fatalf("insights = %v", ids)
	}
}

func TestTagsAndTagFilter(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	srv.Tags = Tags{"client-a": {"/p/one"}}
	payload := dashboardPayload(t, get(t, srv, "/").Body.String())
	tags := payload["tags"].([]any)
	if len(tags) != 2 || tags[0].(map[string]any)["tag"] != "client-a" || tags[0].(map[string]any)["cost"].(float64) != 4.5 {
		t.Fatalf("tags = %v", tags)
	}
	filtered := dashboardPayload(t, get(t, srv, "/?tag=client-a").Body.String())
	if filtered["totalCost"] != nil && filtered["totalCost"].(float64) != 4.5 {
		t.Fatalf("tag filter total = %v", filtered["totalCost"])
	}
	if projects := filtered["projects"].([]any); len(projects) != 1 {
		t.Fatalf("tag filter kept %d projects", len(projects))
	}
}

func TestComparisonInPayload(t *testing.T) {
	srv, st := newTestServer(t)
	srv.Generated = time.Now
	seed(t, st, time.Now())
	cmp := dashboardPayload(t, get(t, srv, "/").Body.String())["comparison"].(map[string]any)
	if cmp["current"].(map[string]any)["cost"].(float64) != 4.5 || cmp["label"] != "Last 7 days" {
		t.Fatalf("comparison = %v", cmp)
	}
}

func TestDigestAndNotifier(t *testing.T) {
	_, st := newTestServer(t)
	now := time.Now()
	seed(t, st, now)
	md, err := BuildDigest(context.Background(), st, DigestOptions{Days: 7, Now: now})
	if err != nil || !strings.Contains(md, "dashd digest") || !strings.Contains(md, "$4.50") {
		t.Fatalf("digest = %q, %v", md, err)
	}

	var hits atomic.Int32
	var last string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		last = string(b)
	}))
	defer hook.Close()
	cfg := NotifierConfig{WebhookURL: hook.URL, DailyBudget: 1, DigestEvery: 24 * time.Hour, Now: func() time.Time { return now }}
	if err := NotifyOnce(context.Background(), st, cfg); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 || !strings.Contains(last, `"content"`) {
		t.Fatalf("first pass sent %d webhooks (want budget alert + digest), last %q", hits.Load(), last)
	}
	// The same day: neither repeats.
	if err := NotifyOnce(context.Background(), st, cfg); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("alerts repeated: %d", hits.Load())
	}
}

func TestPayloadCacheInvalidatesOnIngest(t *testing.T) {
	srv, st := newTestServer(t)
	srv.CachePayloads = true
	seed(t, st, time.Now())
	first := get(t, srv, "/").Body.String()
	if rec := post(srv.Handler(), "/api/ingest", ingestBody(t, oneSession("new-one", "m1", 1)), nil); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	second := get(t, srv, "/").Body.String()
	if first == second {
		t.Fatal("cached payload survived an ingest")
	}
}
