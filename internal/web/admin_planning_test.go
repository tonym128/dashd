package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/source"
	"github.com/tonym128/dashd/internal/store"
)

type fakePrices struct {
	set     map[string]source.Rates
	removed string
	err     error
}

func (f *fakePrices) SetPrice(_ context.Context, m string, r source.Rates, _ bool) (int, error) {
	f.set[m] = r
	return 3, f.err
}

func (f *fakePrices) RemovePrice(_ context.Context, m string) (int, error) {
	f.removed = m
	return 1, f.err
}

func adminPost(srv *Server, path, body, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPlanningAdminEndpoints(t *testing.T) {
	srv, st := newTestServer(t)
	local := "127.0.0.1:1"
	if rec := adminPost(srv, "/api/admin/price", `{"model":"m"}`, local); rec.Code != http.StatusNotImplemented {
		t.Errorf("no price admin: %d", rec.Code)
	}
	prices := &fakePrices{set: map[string]source.Rates{}}
	srv.PriceAdmin = prices
	for _, tc := range []struct {
		path, body, remote string
		want               int
	}{
		{"/api/admin/price", `{"model":"m","input":2,"output":3}`, local, 200},
		{"/api/admin/price", `{"model":"m","remove":true}`, local, 200},
		{"/api/admin/price", `{"model":""}`, local, 400},
		{"/api/admin/price", `{"model":"m","input":-1}`, local, 400},
		{"/api/admin/price", `{"model":"m","input":2}`, "203.0.113.9:1", 403},
		{"/api/admin/budget", `{"scope":"project","name":"/w","monthly":5}`, local, 200},
		{"/api/admin/budget", `{"scope":"planet","name":"x","monthly":5}`, local, 400},
		{"/api/admin/budget", `{"scope":"project","name":"/w","monthly":5}`, "203.0.113.9:1", 403},
		{"/api/admin/plan", `{"name":"Max","agent":"claude","monthly":200}`, local, 200},
		{"/api/admin/plan", `{"name":"Max","monthly":200}`, local, 400},
		{"/api/admin/plan", `not json`, local, 400},
	} {
		if rec := adminPost(srv, tc.path, tc.body, tc.remote); rec.Code != tc.want {
			t.Errorf("%s %s from %s = %d, want %d", tc.path, tc.body, tc.remote, rec.Code, tc.want)
		}
	}
	if prices.set["m"].Input != 2 || prices.removed != "m" {
		t.Errorf("%+v", prices)
	}
	prices.err = errors.New("disk full")
	if rec := adminPost(srv, "/api/admin/price", `{"model":"m"}`, local); rec.Code != 500 || strings.Contains(rec.Body.String(), "disk") {
		t.Errorf("failure: %d %s", rec.Code, rec.Body)
	}
	if b, _ := st.Budgets(context.Background()); len(b) != 1 {
		t.Errorf("budgets %+v", b)
	}
	body := get(t, srv, "/?view=planning").Body.String()
	for _, want := range []string{"Max", `data-remove-budget="/w"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	_ = adminPost(srv, "/api/admin/plan", `{"name":"Max","monthly":0}`, local)
	if p, _ := st.Plans(context.Background()); len(p) != 0 {
		t.Errorf("plans %+v", p)
	}
}

func TestPricesPanelReportsFailuresAndChanges(t *testing.T) {
	srv, st := newTestServer(t)
	now := srv.Generated()
	srv.PriceStatus = &PriceStatus{}
	srv.PriceStatus.Configure(24*time.Hour, 10)
	srv.PriceStatus.Attempt(now.Add(-time.Hour), 12, nil)
	srv.PriceStatus.Attempt(now.Add(-time.Minute), 0, errors.New("permission denied"))
	if err := st.RecordPrices([]store.PricePoint{{Model: "steady-model", Rates: `{"input":1}`, Source: "catalogue"},
		{Model: "changed", ValidFrom: now.Unix(), Rates: `{"input":1234.5}`, Source: "catalogue"},
		{Model: "broken", ValidFrom: 5, Rates: `nope`, Source: "catalogue"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetManualPrice("mine", `{"input":0.075}`, now, true); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/?view=system").Body.String()
	for _, want := range []string{"permission denied", "12 models", "$1,234.5", "$0.075", `data-remove-price="mine"`, "price list is not refreshing"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "<td>steady-model</td>") {
		t.Error("a price that never changed is listed as a change")
	}
}

func TestIngestStoresCommits(t *testing.T) {
	srv, st := newTestServer(t)
	now := time.Now()
	spend(t, st, "a", "/w/a", now.Add(-time.Hour), 5)
	ok := `{"machine":"box","sessions":[],"commits":[{"project":"/w/a","hash":"abcdef123456","ts":` + itoa(int(now.Unix())) + `,"subject":"<b>fix</b>"}]}`
	if rec := adminPost(srv, "/api/ingest", ok, "127.0.0.1:1"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{`{"project":"","hash":"abcdef1","ts":1}`, `{"project":"/w","hash":"x","ts":1}`, `{"project":"/w","hash":"abcdef1","ts":0}`} {
		if rec := adminPost(srv, "/api/ingest", `{"machine":"box","commits":[`+bad+`]}`, "127.0.0.1:1"); rec.Code != 400 {
			t.Errorf("%s accepted: %d", bad, rec.Code)
		}
	}
	body := get(t, srv, "/?view=planning").Body.String()
	if !strings.Contains(body, "abcdef12") || !strings.Contains(body, "&lt;b&gt;fix") || !strings.Contains(body, "$5.00") {
		t.Error("the commit is not on the page, or is not escaped")
	}
}

func TestRateLimitsReachThePage(t *testing.T) {
	if len(rateLimitInsights(2)) != 0 || len(rateLimitInsights(3)) != 1 {
		t.Error("the insight threshold is 3 in a week")
	}
	v := rateLimitViews([]store.RateLimitRow{{SessionUID: "a b", TS: 1, Agent: "claude"}})
	if len(v) != 1 || v[0].URL != "/session?uid=a+b" {
		t.Errorf("%+v", v)
	}
	if c := commitViews([]store.CommitCost{{}}); c[0].Subject == "" {
		t.Error("a redacted subject is blank")
	}
}

func TestManyFailingMachinesAreOneInsight(t *testing.T) {
	m := []machineStatusView{{Machine: "a", LastError: "x"}, {Machine: "b", LastError: "y"}}
	if got := BuildInsightsFull(InsightInputs{Machines: m}); len(got) != 1 || !strings.Contains(got[0].Headline, "2 machines") {
		t.Errorf("%+v", got)
	}
}
