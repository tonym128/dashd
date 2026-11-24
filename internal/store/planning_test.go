package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

func TestPlanningTablesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(filepath.Join(t.TempDir(), "p.db"))
	defer st.Close()
	at := time.Now().Add(-time.Hour)
	if err := st.ReplaceSession(model.SessionWrite{Session: model.Session{UID: "u", Agent: "claude", Project: "/w", Machine: "box"},
		RateLimits: []model.RateLimit{{Time: at}, {}},
		Calls: []model.Call{
			{SessionUID: "u", CallKey: "a", Agent: "claude", Project: "/w", Machine: "box", Model: "cheap", Time: at, Day: at.Format("2006-01-02"), InputTokens: 7, OutputTokens: 2, ReasoningTokens: 3, CostUSD: 1, Priced: true},
			{SessionUID: "u", CallKey: "b", Agent: "claude", Project: "/w", Machine: "box", Model: "dear", Time: at, Day: at.Format("2006-01-02"), CostUSD: 9, Priced: true},
		}}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []Budget{{"machine", "box", 5}, {"agent", "claude", 9}, {"agent", "claude", 0}} {
		if err := st.SetBudget(b); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []Plan{{"Max", "claude", 200}, {"Pro", "codex", 20}, {"Pro", "", 0}} {
		if err := st.SetPlan(p); err != nil {
			t.Fatal(err)
		}
	}
	if p, err := st.Plans(ctx); err != nil || len(p) != 1 || p[0].Monthly != 200 {
		t.Errorf("plans %+v %v", p, err)
	}
	_ = st.SaveCommits("box", []model.Commit{{Project: "/w", Hash: "abcdefgh", TS: at.Unix() + 1}})
	if _, err := st.RenameMachine("box", "desk"); err != nil {
		t.Fatal(err)
	}
	if b, err := st.Budgets(ctx); err != nil || len(b) != 1 || b[0].Name != "desk" {
		t.Errorf("budgets %+v %v", b, err)
	}
	if rl, n, _ := st.RateLimits(ctx, Filter{Machines: []string{"desk"}}, 5); n != 1 || len(rl) != 1 {
		t.Errorf("%d rate limits followed the rename", n)
	}
	if m, err := st.SessionModels(ctx, []string{"u"}); err != nil || len(m["u"]) != 2 || m["u"][0] != "dear" {
		t.Errorf("models %v %v", m, err)
	}
	if m, err := st.SessionModels(ctx, nil); err != nil || len(m) != 0 {
		t.Errorf("%v %v", m, err)
	}
	if d, err := st.DimDays(ctx, Filter{}, at.Add(-time.Minute)); err != nil || len(d) != 2 || d[0].Machine != "desk" {
		t.Errorf("dim days %+v %v", d, err)
	}
	out := int64(0)
	if err := st.EachCallTokens(ctx, Filter{Models: []string{"cheap"}}, func(_ string, _, o, _, _, _ int64, _ float64, _ bool) { out += o }); err != nil || out != 5 {
		t.Errorf("out %d %v, want output plus reasoning", out, err)
	}

	if err := st.RecordPrices([]PricePoint{{Model: "m", Rates: "{}", Source: "catalogue"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetManualPrice("m", `{"input":1}`, at, false); err != nil {
		t.Fatal(err)
	}
	if h, err := st.PriceHistory(ctx); err != nil || len(h) != 2 || h[1].Source != "manual" {
		t.Errorf("history %+v %v", h, err)
	}
	if err := st.ClearPrices(""); err != nil {
		t.Fatal(err)
	}
	if h, _ := st.PriceHistory(ctx); len(h) != 1 {
		t.Errorf("clearing the catalogue left %+v", h)
	}
	if _, err := st.PruneBefore(ctx, time.Now(), PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.CommitCosts(ctx, Filter{}, 5); len(c) != 0 {
		t.Errorf("prune kept %+v", c)
	}
}
