package web

import (
	"strings"
	"testing"
)

// app.js runs in the same harness as dashboard.js, loaded after it, so its
// renderers are executed rather than grepped.
func TestAppJSRendersComparisonAndTagsEscaped(t *testing.T) {
	payload := sentinelPayload()
	payload["timezone"] = "Australia/Sydney (AEDT)"
	payload["comparison"] = map[string]any{
		"label": "Last 7 days", "currentFrom": "2026-11-12", "currentTo": "2026-11-18",
		"priorFrom": "2026-11-05", "priorTo": "2026-11-11",
		"current": map[string]any{"cost": 20.0, "calls": 10, "sessions": 2, "tokens": 100, "per_call": 2.0},
		"prior":   map[string]any{"cost": 10.0, "calls": 5, "sessions": 1, "tokens": 50, "per_call": 2.0},
		"models":  []any{map[string]any{"name": `<img src=x onerror=alert(1)>`, "current": 20.0, "prior": 10.0, "delta": 10.0}},
	}
	payload["tags"] = []any{map[string]any{"tag": `"><script>`, "projects": 1, "sessions": 2, "calls": 3,
		"cost": 4.5, "per_session": 2.25, "pct": 100.0}}

	const body = `async function () {
        return {
            cmp: document.getElementById('comparison-content').innerHTML,
            tags: document.getElementById('tags-tbody') ? document.getElementById('tags-tbody').innerHTML : null,
            tz: document.getElementById('tz-badge').textContent,
        };
    }`
	ids := append(templateIDs(t), "tags-tbody")
	var got struct {
		Cmp  string `json:"cmp"`
		Tags string `json:"tags"`
		TZ   string `json:"tz"`
	}
	runDashboardJS(t, jsConfig{Ids: ids, Data: payload, Activity: sentinelActivity(),
		Sessions: sentinelSessions(), Extra: []string{"app.js"}}, body, &got)

	if !strings.Contains(got.Cmp, "+100%") || !strings.Contains(got.Cmp, "$20.00") {
		t.Errorf("comparison did not render the change: %s", got.Cmp)
	}
	if strings.Contains(got.Cmp, "<img") || !strings.Contains(got.Cmp, "&lt;img") {
		t.Errorf("comparison rendered a model name raw: %s", got.Cmp)
	}
	if strings.Contains(got.Tags, "<script>") || !strings.Contains(got.Tags, "&lt;script&gt;") {
		t.Errorf("tags table rendered a tag raw: %s", got.Tags)
	}
	if got.TZ != "Australia/Sydney (AEDT)" {
		t.Errorf("timezone badge = %q", got.TZ)
	}
}

func TestNewScriptsHaveNoInlineHandlersAndAreServed(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, name := range []string{"app.js", "session.js"} {
		script := stripLineComments(readAsset(t, assets, "assets/"+name))
		for _, h := range []string{" onclick=", " onerror=", " onload=", "javascript:"} {
			if strings.Contains(script, h) {
				t.Errorf("%s contains %q; the CSP forbids inline handlers", name, h)
			}
		}
		rec := get(t, srv, "/assets/"+name)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
			t.Errorf("/assets/%s = %d %q", name, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}
