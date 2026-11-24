package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestBareVisitLandsOnTheRememberedRange(t *testing.T) {
	srv, _ := newTestServer(t)
	for cookie, want := range map[string]string{"": "/?range=30d", "7d": "/?range=7d", "all": "/?range=all", "evil": "/?range=30d"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept", "text/html")
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: rangeCookie, Value: cookie})
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
			t.Errorf("cookie %q: %d %q, want %q", cookie, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}

func TestRollingRangesResolveAndExplicitDatesWin(t *testing.T) {
	f, st, err := parseFilter(url.Values{"range": {"7d"}})
	if err != nil || f.DateFrom == nil || st.Range != "7d" || st.Active {
		t.Fatalf("range=7d: %+v %+v %v", f, st, err)
	}
	if want := localDay(time.Now()).AddDate(0, 0, -6); !f.DateFrom.Equal(want) {
		t.Errorf("from %v, want %v", f.DateFrom, want)
	}
	f, st, _ = parseFilter(url.Values{"range": {"7d"}, "date_from": {"2026-01-02"}})
	if f.DateFrom.Format("2006-01-02") != "2026-01-02" || f.DateTo != nil || st.Range != "" {
		t.Errorf("explicit date lost to the range: %+v", f)
	}
	for _, r := range []string{"all", "", "bogus"} {
		if f, _, _ := parseFilter(url.Values{"range": {r}}); f.DateFrom != nil {
			t.Errorf("range %q set a date", r)
		}
	}
}
