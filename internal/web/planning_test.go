package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
)

func spend(t *testing.T, st *store.Store, uid, project string, at time.Time, cost float64) {
	t.Helper()
	if err := st.AppendSession(model.SessionWrite{Session: model.Session{UID: uid, Agent: "claude", Project: project, Machine: "box"},
		Calls: []model.Call{{SessionUID: uid, CallKey: at.String(), Agent: "claude", Project: project, Model: "m", Machine: "box",
			Time: at, Day: at.Local().Format("2006-01-02"), InputTokens: 1000, OutputTokens: 10, ReasoningTokens: 5, CostUSD: cost, Priced: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession(uid); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetsMeasureTheirOwnScope(t *testing.T) {
	_, st := newTestServer(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	spend(t, st, "a", "/w/a", now.AddDate(0, 0, -1), 60)
	spend(t, st, "b", "/w/b", now.AddDate(0, 0, -1), 5)
	spend(t, st, "a", "/w/a", now.AddDate(0, -1, 0), 900) // last month
	for _, b := range []store.Budget{{Scope: "project", Name: "/w/a", Monthly: 50}, {Scope: "project", Name: "/w/b", Monthly: 1000},
		{Scope: "tag", Name: "t", Monthly: 100}, {Scope: "tag", Name: "none", Monthly: 1}, {Scope: "agent", Name: "claude", Monthly: 70}} {
		if err := st.SetBudget(b); err != nil {
			t.Fatal(err)
		}
	}
	views, err := budgetViews(ctx, st, Tags{"t": {"/w/b"}, "none": {"/nowhere"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range views {
		got[v.Scope+" "+v.Name] = fmt.Sprintf("%.0f %s", v.Spent, v.State)
	}
	want := map[string]string{"project /w/a": "60 over", "project /w/b": "5 ok", "tag t": "5 ok", "tag none": "0 ok", "agent claude": "65 forecast"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	if n := len(budgetInsights(views)); n != 2 {
		t.Errorf("%d budget insights, want 2", n)
	}
	if err := st.SetBudget(store.Budget{Scope: "agent", Name: "claude"}); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.Budgets(ctx); len(left) != 4 {
		t.Errorf("%d budgets after removing one, want 4", len(left))
	}
}

func TestBudgetAlertFiresOncePerState(t *testing.T) {
	_, st := newTestServer(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	spend(t, st, "a", "/w/a", now.AddDate(0, 0, -1), 60)
	_ = st.SetBudget(store.Budget{Scope: "project", Name: "/w/a", Monthly: 50})
	sent := 0
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent++ }))
	defer hook.Close()
	cfg := NotifierConfig{WebhookURL: hook.URL, Client: hook.Client(), Now: func() time.Time { return now }}
	for range 2 {
		if err := NotifyOnce(context.Background(), st, cfg); err != nil {
			t.Fatal(err)
		}
	}
	if sent != 1 {
		t.Errorf("%d alerts, want 1", sent)
	}
}

func TestPlansCompareFeeWithUsage(t *testing.T) {
	_, st := newTestServer(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	spend(t, st, "a", "/w/a", now.AddDate(0, 0, -3), 500)
	_ = st.SetPlan(store.Plan{Name: "Max", Agent: "claude", Monthly: 200})
	v, err := planViews(context.Background(), st, now)
	if err != nil || len(v) != 1 || v[0].Multiple != "2.5×" || !v[0].Better {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestSpikesNeedHistoryAndSize(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	var days []store.DimDay
	add := func(ago int, project string, cost float64) {
		days = append(days, store.DimDay{Day: now.AddDate(0, 0, -ago).Format("2006-01-02"), Project: project, Cost: cost})
	}
	for i := 2; i < 10; i++ {
		add(i, "/w/steady", 5)
		add(i, "/w/tiny", 0.1)
	}
	add(0, "/w/steady", 50)
	add(0, "/w/tiny", 3) // 30x, but three dollars
	add(0, "/w/new", 500)
	got := spikeInsights(days, now, nil)
	if len(got) != 1 || !strings.Contains(got[0].Headline, "steady") || !strings.Contains(got[0].ActionURL, "date_from=2026-10-15") {
		t.Fatalf("%+v", got)
	}
}

func TestWhatIfRecostsTheFilteredCalls(t *testing.T) {
	srv, st := newTestServer(t)
	now := time.Now().Add(-time.Hour)
	spend(t, st, "a", "/w/a", now, 60)
	spend(t, st, "b", "/w/b", now, 5)
	srv.Pricer = func(m string, in, out, cr, cw, cw1h, ts int64) (float64, bool) {
		return float64(in+out) / 1000, m == "cheap"
	}
	rec := get(t, srv, "/api/whatif?target=cheap&project=/w/a")
	var r map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(rec.Body.String())
	}
	if r["calls"] != 1.0 || r["actual"] != 60.0 || r["hypothetical"] != 1.015 {
		t.Errorf("%v", r)
	}
	if rec := get(t, srv, "/api/whatif?target=unknown"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown model: %d", rec.Code)
	}
}

func TestCacheSavingsIgnoreLongPromptTiers(t *testing.T) {
	price := func(m string, in, out, cr, cw, cw1h, ts int64) (float64, bool) {
		rate := 1.0
		if in+cr > 200_000 {
			rate = 100
		}
		return rate * (float64(in) + 0.1*float64(cr)) / 1e6, true
	}
	got := cacheSavings([]store.ModelStat{{Model: "m", CacheReadTokens: 10_000_000}}, price)
	if got < 8.99 || got > 9.01 {
		t.Errorf("savings = %v, want 9", got)
	}
}
