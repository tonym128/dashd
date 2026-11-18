package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
)

func ingestBody(t *testing.T, req model.IngestRequest) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

func oneSession(uid, machine string, cost float64) model.IngestRequest {
	return model.IngestRequest{Machine: machine, Sessions: []model.IngestSession{{
		Mode: "replace",
		Session: model.SessionWrite{
			Session: model.Session{UID: uid, Agent: model.AgentClaude, Project: "/p/x"},
			Calls: []model.Call{{
				SessionUID: uid, Agent: model.AgentClaude, Model: "claude-sonnet-4-6", CallKey: "c1",
				Time: time.Date(2026, 11, 17, 23, 30, 0, 0, time.UTC), Day: "1999-01-01",
				InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100, CostUSD: cost, Priced: true,
			}},
		},
	}}}
}

func post(h http.Handler, path string, body *bytes.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCrossSiteWritesAreRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"foreign origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"text/plain simple request", map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"same origin", map[string]string{"Origin": "http://example.com"}, http.StatusOK},
		{"non-browser client", nil, http.StatusOK},
	}
	for _, tc := range cases {
		rec := post(h, "/api/ingest", ingestBody(t, oneSession("u-"+tc.name, "m1", 1)), tc.hdr)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestHostCheckBlocksRebinding(t *testing.T) {
	h := WithHostCheck([]string{"dash.lan"}, okHandler{})
	for host, want := range map[string]int{
		"127.0.0.1:8753":      200,
		"[::1]:8753":          200,
		"localhost:8753":      200,
		"app.localhost":       200,
		"192.168.1.20:8753":   200,
		"dash.lan:8753":       200,
		"rebind.evil.example": http.StatusMisdirectedRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: status %d, want %d", host, rec.Code, want)
		}
	}
	if WithHostCheck([]string{"*"}, okHandler{}) != (okHandler{}) {
		t.Error("* should disable the host check")
	}
}

func TestIngestDisabled(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.IngestEnabled = false
	rec := post(srv.Handler(), "/api/ingest", ingestBody(t, oneSession("u1", "m1", 1)), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
}

func TestIngestServerPricesAndRebucketsDay(t *testing.T) {
	srv, st := newTestServer(t)
	srv.Pricer = func(m string, in, out, cr, cw, cw1h int64) (float64, bool) { return 0.5, true }
	rec := post(srv.Handler(), "/api/ingest", ingestBody(t, oneSession("u1", "m1", 999)), nil)
	if rec.Code != 200 {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}
	var resp model.IngestResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.InstanceID == "" {
		t.Error("response carries no instance_id")
	}
	calls, err := st.SessionCalls(t.Context(), "u1")
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls = %v, %v", calls, err)
	}
	if calls[0].Cost != 0.5 {
		t.Errorf("cost = %v, want the server's 0.5 rather than the client's 999", calls[0].Cost)
	}
	var day string
	_ = st.DB().QueryRow(`SELECT day FROM call WHERE session_uid = 'u1'`).Scan(&day)
	want := time.Date(2026, 11, 17, 23, 30, 0, 0, time.UTC).Local().Format("2006-01-02")
	if day != want {
		t.Errorf("day = %q, want %q derived on the server, not the client's", day, want)
	}
}

func TestIngestCannotOverwriteAnotherMachinesSession(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Handler()
	if rec := post(h, "/api/ingest", ingestBody(t, oneSession("shared", "alpha", 1)), nil); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := post(h, "/api/ingest", ingestBody(t, oneSession("shared", "beta", 2)), nil); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	a, ok, _ := st.Session(t.Context(), "shared")
	if !ok || a.Machine != "alpha" {
		t.Fatalf("alpha's session = %+v, %v; want it untouched", a, ok)
	}
	b, ok, _ := st.Session(t.Context(), "beta::shared")
	if !ok || b.Machine != "beta" {
		t.Fatalf("beta's session not namespaced: %+v %v", b, ok)
	}
}

func TestIngestRejectsBadMachineNames(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := post(srv.Handler(), "/api/ingest", ingestBody(t, oneSession("u1", "<script>", 1)), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}

func TestMachineBoundTokenForcesMachine(t *testing.T) {
	srv, st := newTestServer(t)
	h := WithAuthConfig(AuthConfig{UIToken: "ui-token-0123456789",
		MachineTokens: map[string]string{"laptop-token-0123456789": "laptop"}, Log: discardLog()}, srv.Handler())
	rec := post(h, "/api/ingest", ingestBody(t, oneSession("u1", "someone-else", 1)),
		map[string]string{"Authorization": "Bearer laptop-token-0123456789"})
	if rec.Code != 200 {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}
	s, ok, _ := st.Session(t.Context(), "u1")
	if !ok || s.Machine != "laptop" {
		t.Fatalf("machine = %q, want the token's bound machine", s.Machine)
	}
	// A machine token cannot read the dashboard.
	req := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	req.Header.Set("Authorization", "Bearer laptop-token-0123456789")
	r2 := httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != http.StatusUnauthorized {
		t.Fatalf("machine token on UI = %d, want 401", r2.Code)
	}
}

func TestBrowserLoginFlow(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	srv.RepriceFunc = func(context.Context) (int, error) { return 0, nil }
	h := WithAuthConfig(AuthConfig{UIToken: testToken, Log: discardLog()}, srv.Handler())

	// A browser navigation is redirected to the login form, not given a 401.
	req := httptest.NewRequest(http.MethodGet, "/?project=x", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
		t.Fatalf("browser GET / = %d %q, want a redirect to /login", rec.Code, rec.Header().Get("Location"))
	}

	// Wrong token: no cookie.
	form := strings.NewReader("token=wrong&next=/")
	req = httptest.NewRequest(http.MethodPost, "/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("bad login = %d with %d cookies", rec.Code, len(rec.Result().Cookies()))
	}

	// Right token: a strict, HttpOnly cookie and a redirect to a local path only.
	form = strings.NewReader("token=" + testToken + "&next=//evil.example/")
	req = httptest.NewRequest(http.MethodPost, "/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusSeeOther || len(cookies) != 1 || rec.Header().Get("Location") != "/" {
		t.Fatalf("login = %d, location %q, %d cookies", rec.Code, rec.Header().Get("Location"), len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || strings.Contains(c.Value, testToken) {
		t.Fatalf("cookie = %+v; want HttpOnly, SameSite=Strict and no raw token", c)
	}

	// The cookie reads the dashboard.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET / with cookie = %d", rec.Code)
	}

	// A cookie-authenticated POST needs the CSRF header.
	req = httptest.NewRequest(http.MethodPost, "/api/reprice", nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie POST without X-Dashd-Request = %d, want 403", rec.Code)
	}
	// A forged cookie is worthless.
	req = httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "admin.deadbeef"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie = %d, want 401", rec.Code)
	}
}

func TestAdminTokenSeparatesScopes(t *testing.T) {
	srv, _ := newTestServer(t)
	called := 0
	srv.RepriceFunc = func(context.Context) (int, error) {
		called++
		return 0, nil
	}
	h := WithAuthConfig(AuthConfig{UIToken: "ui-secret", AdminToken: "admin-secret", Log: discardLog()}, srv.Handler())
	for tok, want := range map[string]int{"ui-secret": http.StatusUnauthorized, "admin-secret": 200} {
		req := httptest.NewRequest(http.MethodPost, "/api/reprice", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("reprice with %s = %d, want %d", tok, rec.Code, want)
		}
	}
	if called != 1 {
		t.Errorf("reprice ran %d times, want 1", called)
	}
}

func TestAdminWithoutTokenIsLoopbackOnly(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	h := srv.Handler()
	body := func() *bytes.Reader { return bytes.NewReader([]byte(`{"uid":"sess-a"}`)) }
	rec := post(h, "/api/admin/forget-session", body(), nil) // httptest's RemoteAddr is 192.0.2.1
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote admin without a token = %d, want 403", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/forget-session", body())
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:5555"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("loopback forget = %d %s", rec.Code, rec.Body.String())
	}
	if ok, _ := st.SessionExists("sess-a"); ok {
		t.Error("session still present after forget")
	}
}

func TestAdminRenameMachineAndPrune(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Handler()
	if rec := post(h, "/api/ingest", ingestBody(t, oneSession("u1", "old-name", 1)), nil); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/rename-machine", strings.NewReader(`{"from":"old-name","to":"new-name"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body.String())
	}
	m, _ := st.Machines(t.Context())
	if len(m) != 1 || m[0] != "new-name" {
		t.Fatalf("machines = %v", m)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/admin/prune", strings.NewReader(`{"before":"2030-01-01"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"pruned":1`) {
		t.Fatalf("prune = %d %s", rec.Code, rec.Body.String())
	}
}

func TestThrottledLog(t *testing.T) {
	var tl throttledLog
	tl.every = time.Minute
	now := time.Now()
	if !tl.allow("a", now) || tl.allow("a", now.Add(time.Second)) || !tl.allow("a", now.Add(2*time.Minute)) {
		t.Fatal("throttle did not allow once per interval")
	}
}

var _ = store.Filter{}

// A browser submitting the login form from a page with a no-referrer policy
// sends "Origin: null" with Sec-Fetch-Site: same-origin; behind a proxy the
// Host may differ from the Origin. Both are same-origin and must be allowed.
func TestLoginFormFromABrowserIsNotRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	h := WithAuthConfig(AuthConfig{UIToken: testToken, Log: discardLog()}, srv.Handler())
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"origin null, same-origin fetch metadata", map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-origin"}, http.StatusSeeOther},
		{"proxy rewrote Host, fetch metadata", map[string]string{"Origin": "https://dash.ts.net", "Sec-Fetch-Site": "same-origin"}, http.StatusSeeOther},
		{"proxy rewrote Host, X-Forwarded-Host", map[string]string{"Origin": "https://dash.ts.net", "X-Forwarded-Host": "dash.ts.net"}, http.StatusSeeOther},
		{"cross-site with a forged Origin", map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site subdomain", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"origin null without fetch metadata", map[string]string{"Origin": "null"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token="+testToken+"&next=/"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range tc.hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
		}
	}
}
