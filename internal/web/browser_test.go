package web

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

// Browser tests: the real page, laid out by a real browser.
//
// The node harness runs the scripts without layout, so it cannot see a section
// that CSS leaves visible, overlapping controls, or a hidden attribute a
// display rule overrides. These load the dashboard in an iframe of a probe page
// and read back what the browser computed, via headless Chrome's --dump-dom, so
// no driver or dependency is needed. Set DASHD_REQUIRE_BROWSER=1 (CI does) to
// fail rather than skip when no browser is installed.

func findBrowser(t *testing.T) string {
	t.Helper()
	for _, n := range []string{os.Getenv("DASHD_BROWSER"), "chromium", "google-chrome", "chrome", "chromium-browser"} {
		if n == "" {
			continue
		}
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	if os.Getenv("DASHD_REQUIRE_BROWSER") != "" {
		t.Fatal("no Chrome or Chromium found")
	}
	t.Skip("no Chrome or Chromium found")
	return ""
}

var probeRe = regexp.MustCompile(`(?s)<pre id="probe">(.*?)</pre>`)

// probe loads path at a width and returns the JSON value of body, the body of
// an async JavaScript function given doc, win and sleep(ms).
func probe(t *testing.T, h http.Handler, width int, path, body string, out any) {
	t.Helper()
	browser := findBrowser(t)
	page := fmt.Sprintf(`<!doctype html><iframe id="f" width="%d" height="900" src="%s"></iframe><pre id="probe"></pre>
<script>
const f = document.getElementById('f');
const sleep = ms => new Promise(r => setTimeout(r, ms));
f.onload = async () => {
  f.onload = null;
  let v;
  try { await sleep(400); v = await (async (win, sleep) => { const doc = win.document; %s })(f.contentWindow, sleep); }
  catch (e) { v = { error: String(e && e.stack || e) }; }
  document.getElementById('probe').textContent = JSON.stringify(v === undefined ? null : v);
};
</script>`, width, html.EscapeString(path), body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__probe" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, page)
			return
		}
		h.ServeHTTP(&framable{ResponseWriter: w}, r)
	}))
	defer srv.Close()
	profile := t.TempDir()
	cmd := exec.Command(browser, "--headless", "--disable-gpu", "--no-sandbox", "--no-first-run",
		"--disable-dev-shm-usage", "--user-data-dir="+filepath.Join(profile, "p"), "--window-size=1600,1000",
		"--virtual-time-budget=15000", "--dump-dom", srv.URL+"/__probe")
	timer := time.AfterFunc(90*time.Second, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	dom, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser: %v", err)
	}
	m := probeRe.FindSubmatch(dom)
	if m == nil || len(m[1]) == 0 {
		t.Fatalf("the probe never finished:\n%.2000s", dom)
	}
	raw := html.UnescapeString(string(m[1]))
	if strings.HasPrefix(raw, `{"error"`) {
		t.Fatal(raw)
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("probe returned %s: %v", raw, err)
	}
}

// framable drops the headers that (rightly) forbid framing the dashboard.
type framable struct {
	http.ResponseWriter
	wrote bool
}

func (f *framable) WriteHeader(code int) {
	if !f.wrote {
		f.wrote = true
		f.Header().Del("X-Frame-Options")
		csp := f.Header().Get("Content-Security-Policy")
		f.Header().Set("Content-Security-Policy", strings.Replace(csp, "frame-ancestors 'none'", "frame-ancestors *", 1))
	}
	f.ResponseWriter.WriteHeader(code)
}

func (f *framable) Write(b []byte) (int, error) {
	if !f.wrote {
		f.WriteHeader(http.StatusOK)
	}
	return f.ResponseWriter.Write(b)
}

func browserServer(t *testing.T) http.Handler {
	t.Helper()
	srv, st := newTestServer(t)
	srv.Generated = time.Now
	now := time.Now()
	for i, agent := range []string{"claude", "codex"} {
		var calls []model.Call
		for d := 0; d < 20; d++ {
			ts := now.AddDate(0, 0, -d).Add(-time.Minute)
			calls = append(calls, model.Call{CallKey: fmt.Sprint(d), Agent: agent, Model: agent + "-some-long-model-name-5.5", Time: ts,
				Day: ts.Format("2006-01-02"), InputTokens: 766_100_000, OutputTokens: 24_100_000,
				CacheReadTokens: 811_500_000, CacheWriteTokens: 29_700_000, ReasoningTokens: 13_900_000, TotalTokens: 1_645_300_000,
				LLMSeconds: 2, CostUSD: 1234.5, Priced: true})
		}
		uid := fmt.Sprint("s", i)
		for j := range calls {
			calls[j].SessionUID = uid
		}
		if err := st.ReplaceSession(model.SessionWrite{Session: model.Session{UID: uid, Agent: agent, Project: "/w/" + agent, Machine: "box", Title: "title " + agent}, Calls: calls}); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
	}
	return srv.Handler()
}

func TestBrowserTabsShowOnlyTheirOwnSections(t *testing.T) {
	h := browserServer(t)
	for _, view := range []string{"overview", "insights", "sessions", "breakdown", "system"} {
		var stray []string
		probe(t, h, 1440, "/?view="+view, `
			return [...doc.querySelectorAll('[data-view-section]')]
			  .filter(e => e.offsetParent && !e.dataset.viewSection.split(' ').includes('`+view+`'))
			  .map(e => e.dataset.viewSection + ':' + e.textContent.trim().slice(0, 30));`, &stray)
		if len(stray) > 0 {
			t.Errorf("%s also shows %q", view, stray)
		}
	}
}

func TestBrowserFilterBar(t *testing.T) {
	h := browserServer(t)
	var got struct {
		Program  string
		Checked  int
		Hidden   []string
		ApplyBg  string
		Overflow int
	}
	js := `
		const shown = e => e && e.offsetParent !== null;
		return {
		  Program: doc.querySelector('#agent + details summary').textContent,
		  Checked: doc.querySelectorAll('#agent + details input:checked').length,
		  Hidden: [...doc.querySelectorAll('[hidden]')].filter(shown).map(e => e.id),
		  ApplyBg: win.getComputedStyle(doc.querySelector('#filter-form [type=submit]')).backgroundColor,
		  Overflow: doc.documentElement.scrollWidth - win.innerWidth,
		};`
	probe(t, h, 1440, "/?agent=claude", js, &got)
	if got.Program != "1 selected" || got.Checked != 1 {
		t.Errorf("picker says %q with %d checked while a filter is active", got.Program, got.Checked)
	}
	if len(got.Hidden) > 0 {
		t.Errorf("hidden elements are visible: %q", got.Hidden)
	}
	if got.ApplyBg != "rgb(88, 166, 255)" && got.ApplyBg != "rgb(9, 105, 218)" {
		t.Errorf("Apply is not a primary button: %s", got.ApplyBg)
	}
	var overlaps []string
	probe(t, h, 390, "/?agent=claude", `
		const boxes = [...doc.querySelectorAll('#filter-form .filter-group')].map(e => e.getBoundingClientRect());
		const out = [];
		boxes.forEach((a, i) => boxes.forEach((b, j) => {
		  if (i < j && a.left < b.right - 1 && b.left < a.right - 1 && a.top < b.bottom - 1 && b.top < a.bottom - 1) out.push(i + '/' + j);
		}));
		if (doc.documentElement.scrollWidth > win.innerWidth) out.push('page scrolls sideways: ' + [...doc.querySelectorAll('body *')].filter(e => e.getBoundingClientRect().right > win.innerWidth + 1 && !e.closest('.section, .view-tabs, .sub-tabs')).map(e => e.tagName + '.' + e.className).slice(0, 4));
		if (boxes.some(b => b.width < 200)) out.push('squashed');
		return out;`, &overlaps)
	if len(overlaps) > 0 {
		t.Errorf("phone filter bar: %q", overlaps)
	}
}

func TestBrowserDailyChartShowAll(t *testing.T) {
	var n []int
	probe(t, browserServer(t), 1440, "/?view=overview", `
		const bars = () => doc.querySelectorAll('.daily-bar').length;
		const before = bars();
		doc.querySelector('[data-toggle-daily-chart]').click();
		return [before, bars()];`, &n)
	if len(n) != 2 || n[0] != 14 || n[1] != 20 {
		t.Errorf("bars before/after Show all = %v, want 14, 20", n)
	}
}

func TestBrowserTablesFitAndNumbersAlign(t *testing.T) {
	var bad []string
	probe(t, browserServer(t), 1440, "/?view=breakdown", `
		const out = [];
		doc.querySelectorAll('.section table').forEach(t => {
		  if (!t.offsetParent) return;
		  const box = t.closest('.section');
		  if (t.scrollWidth > box.clientWidth + 1) out.push(t.id + ' is clipped');
		  const wrapped = [...t.querySelectorAll('tbody .model-tag')].some(e => e.getClientRects().length > 1 || e.offsetHeight > 30);
		  if (wrapped) out.push(t.id + ' wraps its names');
		  const heads = [...t.querySelectorAll('thead th')];
		  const row = t.querySelector('tbody tr');
		  if (row && row.cells.length > 1) [...row.cells].forEach((c, i) => {
		    if (heads[i] && win.getComputedStyle(heads[i]).textAlign !== win.getComputedStyle(c).textAlign) out.push(t.id + ' column ' + i + ' misaligned');
		  });
		});
		return out;`, &bad)
	if len(bad) > 0 {
		t.Errorf("%q", bad)
	}
}
