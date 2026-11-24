package web

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/store"
)

// ---------------------------------------------------------------- budgets

type budgetView struct {
	Scope, Name, Label string
	Monthly            float64
	Spent              float64
	Projected          float64
	Pct                float64 // of budget spent, capped for the bar
	State              string  // "over", "forecast", "ok"
	MonthlyText        string
	SpentText          string
	ProjectedText      string
}

// budgetViews measures each budget against this month, whatever the page's
// filter: a budget is about the calendar month.
func budgetViews(ctx context.Context, st *store.Store, tags Tags, now time.Time) ([]budgetView, error) {
	budgets, err := st.Budgets(ctx)
	if err != nil || len(budgets) == 0 {
		return nil, err
	}
	var allProjects []string
	out := make([]budgetView, 0, len(budgets))
	for _, b := range budgets {
		var f store.Filter
		label := b.Name
		switch b.Scope {
		case "project":
			f.Projects = []string{b.Name}
			label = filepath.Base(b.Name)
		case "tag":
			if allProjects == nil {
				if _, _, allProjects, _, _, err = st.Facets(ctx); err != nil {
					return nil, err
				}
			}
			f.Projects = tags.ProjectsFor([]string{b.Name}, allProjects)
			if len(f.Projects) == 0 {
				// An empty list means no filter, which would be everything.
				f.Projects = []string{"\x00no project has this tag"}
			}
		case "model":
			f.Models = []string{b.Name}
		case "machine":
			f.Machines = []string{b.Name}
		case "agent":
			f.Agents = []string{b.Name}
		}
		from := localDay(now).AddDate(0, 0, -31)
		f.DateFrom = &from
		daily, err := st.Daily(ctx, f)
		if err != nil {
			return nil, err
		}
		burn := burnWindows(daily, now)
		fc := monthForecast(burn, now)
		v := budgetView{Scope: b.Scope, Name: b.Name, Label: label, Monthly: b.Monthly, Projected: fc.Projected, State: "ok"}
		for _, w := range burn {
			if w.Label == "Month to date" {
				v.Spent = w.Cost
			}
		}
		v.Pct = math.Min(100, v.Spent/b.Monthly*100)
		switch {
		case v.Spent > b.Monthly:
			v.State = "over"
		case v.Projected > b.Monthly:
			v.State = "forecast"
		}
		v.MonthlyText, v.SpentText, v.ProjectedText = usd(b.Monthly), usd(v.Spent), usd(v.Projected)
		out = append(out, v)
	}
	return out, nil
}

func budgetInsights(views []budgetView) []Insight {
	var out []Insight
	for _, v := range views {
		if v.State == "ok" {
			continue
		}
		in := Insight{
			ID: "budget-" + v.State + "-" + v.Scope + "-" + v.Name, Category: "Cost & Burn", Severity: "warn",
			ActionLabel: "Open Planning", ActionURL: "/?view=planning", Impact: v.Spent,
		}
		if v.State == "over" {
			in.Headline = fmt.Sprintf("%s %s is over its %s monthly budget", v.Scope, v.Label, v.MonthlyText)
			in.Detail = fmt.Sprintf("%s spent this month, %s projected.", v.SpentText, v.ProjectedText)
		} else {
			in.Headline = fmt.Sprintf("%s %s is on track to pass its %s budget", v.Scope, v.Label, v.MonthlyText)
			in.Detail = fmt.Sprintf("%s spent so far, %s projected by month end.", v.SpentText, v.ProjectedText)
		}
		out = append(out, in)
	}
	return out
}

// ---------------------------------------------------------------- plans

type planView struct {
	Name, Agent string
	Monthly     string
	Used        string
	Multiple    string
	Better      bool
}

// planViews compares each subscription's fee with what its agent's usage over
// the last 30 days would cost at list prices.
func planViews(ctx context.Context, st *store.Store, now time.Time) ([]planView, error) {
	plans, err := st.Plans(ctx)
	if err != nil {
		return nil, err
	}
	var out []planView
	for _, p := range plans {
		from := localDay(now).AddDate(0, 0, -29)
		t, err := st.Totals(ctx, store.Filter{Agents: []string{p.Agent}, DateFrom: &from})
		if err != nil {
			return nil, err
		}
		out = append(out, planView{Name: p.Name, Agent: p.Agent, Monthly: usd(p.Monthly), Used: usd(t.Cost),
			Multiple: fmt.Sprintf("%.1f×", t.Cost/p.Monthly), Better: t.Cost > p.Monthly})
	}
	return out, nil
}

// ---------------------------------------------------------------- spikes

// spikeInsights reports the project, model and machine whose spend today or
// yesterday stands out from their own recent days. It needs a week of history
// and a difference worth mentioning, so a first busy day is not a spike.
func spikeInsights(days []store.DimDay, now time.Time, query url.Values) []Insight {
	today := localDay(now)
	todayKey, yestKey := today.Format("2006-01-02"), today.AddDate(0, 0, -1).Format("2006-01-02")
	dims := []struct {
		key, label string
		of         func(store.DimDay) string
	}{
		{"project", "Project", func(d store.DimDay) string { return d.Project }},
		{"model", "Model", func(d store.DimDay) string { return d.Model }},
		{"machine", "Machine", func(d store.DimDay) string { return d.Machine }},
	}
	var out []Insight
	for _, dim := range dims {
		series := map[string]map[string]float64{}
		for _, d := range days {
			name := dim.of(d)
			if name == "" {
				continue
			}
			if series[name] == nil {
				series[name] = map[string]float64{}
			}
			series[name][d.Day] += d.Cost
		}
		var best Insight
		for name, byDay := range series {
			for _, day := range []string{todayKey, yestKey} {
				cost := byDay[day]
				var prior []float64
				for k, v := range byDay {
					if k < yestKey && v > 0 {
						prior = append(prior, v)
					}
				}
				if len(prior) < 5 {
					continue
				}
				sort.Float64s(prior)
				median := prior[len(prior)/2]
				if cost < 3*median || cost-median < 10 || cost-median <= best.Impact {
					continue
				}
				when := "today"
				if day == yestKey {
					when = "yesterday"
				}
				label := name
				if dim.key == "project" {
					label = filepath.Base(name)
				}
				best = Insight{
					ID: "spike-" + dim.key + "-" + name + "-" + day, Category: "Cost & Burn", Severity: "warn",
					Headline:    fmt.Sprintf("%s %s spiked %s: $%.2f", dim.label, label, when, cost),
					Detail:      fmt.Sprintf("%.1f× its usual active day ($%.2f, median of %d days).", cost/median, median, len(prior)),
					ActionLabel: "Show that day",
					ActionURL:   withQuery(query, map[string]string{dim.key: name, "date_from": day, "date_to": day, "range": ""}),
					Impact:      cost - median,
				}
			}
		}
		if best.ID != "" {
			out = append(out, best)
		}
	}
	return out
}

// ---------------------------------------------------------------- what-if

// handleWhatIf re-costs the filtered calls as if another model had answered
// them, call by call so long-prompt tiers apply as they would have.
func (s *Server) handleWhatIf(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	target := strings.TrimSpace(q.Get("target"))
	if s.Pricer == nil || target == "" {
		http.Error(w, "a target model is required", http.StatusBadRequest)
		return
	}
	now := time.Now().Unix()
	if _, ok := s.Pricer(target, 1, 1, 0, 0, 0, now); !ok {
		http.Error(w, "no price is known for that model", http.StatusNotFound)
		return
	}
	filter, _, err := parseFilter(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.applyTagFilter(r.Context(), &filter, q)
	var calls, unpriced int
	var actual, hypothetical float64
	err = s.store.EachCallTokens(r.Context(), filter, func(m string, in, out, cr, cw, cw1h int64, cost float64, priced bool) {
		calls++
		actual += cost
		if !priced {
			unpriced++
		}
		c, _ := s.Pricer(target, in, out, cr, cw, cw1h, now)
		hypothetical += c
	})
	if err != nil {
		s.log.Error("what-if failed", "error", err)
		http.Error(w, "what-if failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"target": target, "calls": calls, "unpriced": unpriced,
		"actual": actual, "hypothetical": hypothetical, "difference": hypothetical - actual})
}

// ---------------------------------------------------------------- admin

func (s *Server) handleAdminBudget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope   string  `json:"scope"`
		Name    string  `json:"name"`
		Monthly float64 `json:"monthly"`
	}
	if !s.adminRequest(w, r, &req) {
		return
	}
	if !slices.Contains(store.BudgetScopes, req.Scope) || req.Name == "" || len(req.Name) > 500 || req.Monthly < 0 {
		http.Error(w, "a scope, a name and an amount of 0 or more are required", http.StatusBadRequest)
		return
	}
	if err := s.store.SetBudget(store.Budget{Scope: req.Scope, Name: req.Name, Monthly: req.Monthly}); err != nil {
		s.log.Error("set budget failed", "error", err)
		http.Error(w, "could not save the budget", http.StatusInternalServerError)
		return
	}
	s.bumpGeneration()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAdminPlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string  `json:"name"`
		Agent   string  `json:"agent"`
		Monthly float64 `json:"monthly"`
	}
	if !s.adminRequest(w, r, &req) {
		return
	}
	if req.Name == "" || len(req.Name) > 100 || (req.Agent == "" && req.Monthly > 0) || req.Monthly < 0 {
		http.Error(w, "a name, a program and a fee of 0 or more are required", http.StatusBadRequest)
		return
	}
	if err := s.store.SetPlan(store.Plan{Name: req.Name, Agent: req.Agent, Monthly: req.Monthly}); err != nil {
		s.log.Error("set plan failed", "error", err)
		http.Error(w, "could not save the plan", http.StatusInternalServerError)
		return
	}
	s.bumpGeneration()
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- commits and rate limits

type commitView struct {
	When, Project, ProjectName, Hash, Subject, Cost, Machine string
	Calls                                                    int
}

const maxCommitRows = 500

// topCommits picks what the page lists: the dearest and the newest, so sorting
// by cost shows the real top of the view and not just of its latest page.
func topCommits(commits []store.CommitCost) []store.CommitCost {
	if len(commits) <= maxCommitRows {
		return commits
	}
	order := make([]int, len(commits))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return commits[order[a]].Cost > commits[order[b]].Cost })
	keep := make(map[int]bool, maxCommitRows)
	for _, i := range order[:maxCommitRows/2] {
		keep[i] = true
	}
	for i := 0; len(keep) < maxCommitRows; i++ {
		keep[i] = true
	}
	out := make([]store.CommitCost, 0, maxCommitRows)
	for i, c := range commits {
		if keep[i] {
			out = append(out, c)
		}
	}
	return out
}

func commitViews(commits []store.CommitCost) []commitView {
	out := make([]commitView, 0, len(commits))
	for _, c := range commits {
		subject := c.Subject
		if subject == "" {
			subject = "(subject not sent)"
		}
		out = append(out, commitView{When: displayTime(c.TS), Project: c.Project, ProjectName: filepath.Base(c.Project),
			Hash: c.Hash[:min(8, len(c.Hash))], Subject: subject, Cost: usd(c.Cost), Machine: c.Machine, Calls: c.Calls})
	}
	return out
}

type rateLimitView struct{ When, Agent, Machine, Detail, URL string }

func rateLimitViews(rows []store.RateLimitRow) []rateLimitView {
	out := make([]rateLimitView, 0, len(rows))
	for _, r := range rows {
		out = append(out, rateLimitView{When: displayTime(r.TS), Agent: r.Agent, Machine: r.Machine, Detail: r.Detail,
			URL: "/session?uid=" + url.QueryEscape(r.SessionUID)})
	}
	return out
}

func rateLimitInsights(week int) []Insight {
	if week < 3 {
		return nil
	}
	return []Insight{{ID: "rate-limits", Category: "Limits", Severity: "tip",
		Headline:    fmt.Sprintf("Rate limits stopped work %d times in the last 7 days", week),
		Detail:      "Each is a request refused for a rate or usage limit. A larger plan, or moving some work to another program, avoids the wait.",
		ActionLabel: "See when", ActionURL: "/?view=planning"}}
}
