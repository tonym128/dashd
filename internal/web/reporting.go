package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/store"
)

// ---------------------------------------------------------------- tags

// Tags maps a tag — a client, team or cost centre — to the project path
// prefixes that belong to it. A project may carry several tags.
type Tags map[string][]string

// LoadTags reads a JSON object of tag -> [prefix, ...]. An empty path yields no
// tags.
func LoadTags(path string) (Tags, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-tags-file: %w", err)
	}
	var t Tags
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("-tags-file %s: %w", path, err)
	}
	return t, nil
}

// Match reports whether project belongs to tag.
func (t Tags) Match(tag, project string) bool {
	for _, prefix := range t[tag] {
		if prefix != "" && strings.HasPrefix(project, prefix) {
			return true
		}
	}
	return false
}

// Names returns the tags, sorted.
func (t Tags) Names() []string {
	out := make([]string, 0, len(t))
	for k := range t {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ProjectsFor expands tags into the matching projects from all.
func (t Tags) ProjectsFor(tags []string, all []string) []string {
	var out []string
	for _, p := range all {
		for _, tag := range tags {
			if t.Match(tag, p) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// tagsJSON rolls per-project cost up by tag.
func tagsJSON(t Tags, projects []store.ProjectStat, totalCost float64) []map[string]any {
	if len(t) == 0 {
		return []map[string]any{}
	}
	type agg struct {
		cost            float64
		calls, sessions int
		projects        map[string]bool
	}
	by := map[string]*agg{}
	for _, name := range t.Names() {
		by[name] = &agg{projects: map[string]bool{}}
	}
	var untaggedCost float64
	var untaggedCalls, untaggedSessions int
	untagged := map[string]bool{}
	for _, p := range projects {
		matched := false
		for name, a := range by {
			if t.Match(name, p.Project) {
				a.cost += p.Cost
				a.calls += p.Calls
				a.sessions += p.Sessions
				a.projects[p.Project] = true
				matched = true
			}
		}
		if !matched {
			untaggedCost += p.Cost
			untaggedCalls += p.Calls
			untaggedSessions += p.Sessions
			untagged[p.Project] = true
		}
	}
	out := make([]map[string]any, 0, len(by)+1)
	row := func(name string, cost float64, calls, sessions, nproj int) map[string]any {
		pct := 0.0
		if totalCost > 0 {
			pct = cost / totalCost * 100
		}
		perSession := 0.0
		if sessions > 0 {
			perSession = cost / float64(sessions)
		}
		return map[string]any{"tag": name, "cost": cost, "calls": calls, "sessions": sessions,
			"projects": nproj, "pct": pct, "per_session": perSession}
	}
	for _, name := range t.Names() {
		a := by[name]
		out = append(out, row(name, a.cost, a.calls, a.sessions, len(a.projects)))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["cost"].(float64) > out[j]["cost"].(float64) })
	if len(untagged) > 0 {
		out = append(out, row("(untagged)", untaggedCost, untaggedCalls, untaggedSessions, len(untagged)))
	}
	return out
}

// ---------------------------------------------------------------- forecast

// forecast projects month-end spend from two run rates: the month so far and
// the last seven days. Their spread is the band, which says how much the
// projection depends on which pace continues.
type forecast struct {
	MTD       float64
	Projected float64
	Low       float64
	High      float64
	DaysLeft  int
	OK        bool
}

func monthForecast(burn []burnWindow, now time.Time) forecast {
	var mtd, last7 burnWindow
	for _, b := range burn {
		switch b.Label {
		case "Month to date":
			mtd = b
		case "Last 7 days":
			last7 = b
		}
	}
	if mtd.Days == 0 || mtd.Cost <= 0 {
		return forecast{}
	}
	today := localDay(now)
	daysInMonth := time.Date(today.Year(), today.Month()+1, 0, 0, 0, 0, 0, time.Local).Day()
	left := daysInMonth - today.Day()
	rateMTD := mtd.Cost / float64(mtd.Days)
	rate7 := rateMTD
	if last7.Days > 0 {
		rate7 = last7.Cost / float64(last7.Days)
	}
	lo, hi := min(rateMTD, rate7), max(rateMTD, rate7)
	return forecast{
		MTD:       mtd.Cost,
		Projected: mtd.Cost + float64(left)*(rateMTD+rate7)/2,
		Low:       mtd.Cost + float64(left)*lo,
		High:      mtd.Cost + float64(left)*hi,
		DaysLeft:  left,
		OK:        true,
	}
}

func forecastCard(f forecast, monthlyBudget float64) burnCard {
	c := burnCard{
		Label:      "Projected month",
		Value:      usd(f.Projected),
		Detail:     groupDollars(fmt.Sprintf("range $%.2f–$%.2f · %d days left", f.Low, f.High, f.DaysLeft)),
		Delta:      "forecast",
		DeltaClass: "burn-flat",
	}
	if monthlyBudget > 0 {
		pct := f.Projected / monthlyBudget * 100
		c.Delta = fmt.Sprintf("%.0f%% of budget", pct)
		switch {
		case pct > 100:
			c.DeltaClass = "burn-up"
		default:
			c.DeltaClass = "burn-down"
		}
	}
	return c
}

// ---------------------------------------------------------------- cache savings

// cacheSavings estimates what cache reads saved: each cached token priced at
// the model's input rate, minus what it was actually billed at the cache-read
// rate. Models without a known rate contribute nothing.
func cacheSavings(models []store.ModelStat, price PriceFunc) float64 {
	if price == nil {
		return 0
	}
	// The rollup has no dates, so savings are valued at today's rates.
	var saved float64
	now := time.Now().Unix()
	for _, m := range models {
		if m.CacheReadTokens <= 0 {
			continue
		}
		// Priced per thousand and scaled: the whole rollup as one prompt would
		// select a long-prompt tier no single call reached.
		asInput, ok1 := price(m.Model, 1000, 0, 0, 0, 0, now)
		asCache, ok2 := price(m.Model, 0, 0, 1000, 0, 0, now)
		if ok1 && ok2 && asInput > asCache {
			saved += (asInput - asCache) * float64(m.CacheReadTokens) / 1000
		}
	}
	return saved
}

// ---------------------------------------------------------------- digest

// DigestOptions configures BuildDigest.
type DigestOptions struct {
	Filter        store.Filter
	Days          int
	DailyBudget   float64
	MonthlyBudget float64
	Now           time.Time
}

// BuildDigest renders a Markdown summary of the last Days days: spend against
// the period before, top projects and models, the month forecast, budget
// status and the actionable insights.
func BuildDigest(ctx context.Context, st *store.Store, o DigestOptions) (string, error) {
	if o.Days <= 0 {
		o.Days = 7
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	end := localDay(o.Now).AddDate(0, 0, 1).Add(-time.Second)
	start := localDay(o.Now).AddDate(0, 0, -(o.Days - 1))
	priorEnd := start.Add(-time.Second)
	priorStart := start.AddDate(0, 0, -o.Days)

	cur := o.Filter
	cur.DateFrom, cur.DateTo = &start, &end
	prior := o.Filter
	prior.DateFrom, prior.DateTo = &priorStart, &priorEnd
	undated := o.Filter
	undated.DateFrom, undated.DateTo = nil, nil

	var (
		tot, priorTot store.Totals
		projects      []store.ProjectStat
		models        []store.ModelStat
		daily         []store.DayBucket
		sig           store.InsightSignals
	)
	err := st.ReadTx(ctx, func(tx *store.Store) error {
		var err error
		if tot, err = tx.Totals(ctx, cur); err != nil {
			return err
		}
		if priorTot, err = tx.Totals(ctx, prior); err != nil {
			return err
		}
		if projects, err = tx.Projects(ctx, cur); err != nil {
			return err
		}
		if models, err = tx.Models(ctx, cur); err != nil {
			return err
		}
		if daily, err = tx.Daily(ctx, undated); err != nil {
			return err
		}
		sig, err = tx.InsightSignals(ctx, cur, tot)
		return err
	})
	if err != nil {
		return "", err
	}
	burn := burnWindows(daily, o.Now)
	fc := monthForecast(burn, o.Now)

	var b strings.Builder
	fmt.Fprintf(&b, "## dashd digest: %s – %s\n\n", start.Format("Jan 2"), end.Format("Jan 2, 2006"))
	delta := "no spend in the prior period"
	if priorTot.Cost > 0 {
		delta = fmt.Sprintf("%+.0f%% vs the %d days before ($%.2f)", (tot.Cost-priorTot.Cost)/priorTot.Cost*100, o.Days, priorTot.Cost)
	}
	fmt.Fprintf(&b, "**Spend:** $%.2f over %d days (%s)\n", tot.Cost, o.Days, delta)
	fmt.Fprintf(&b, "**Activity:** %s calls · %s sessions · %s tokens\n", groupInt64(int64(tot.Calls)), groupInt64(int64(tot.Sessions)), formatCompact(tot.TotalTokens))
	if fc.OK {
		fmt.Fprintf(&b, "**Month forecast:** $%.2f (range $%.2f–$%.2f, $%.2f so far)\n", fc.Projected, fc.Low, fc.High, fc.MTD)
	}
	if o.MonthlyBudget > 0 && fc.OK {
		fmt.Fprintf(&b, "**Monthly budget:** $%.2f — projected %.0f%%\n", o.MonthlyBudget, fc.Projected/o.MonthlyBudget*100)
	}
	if tot.UnpricedCalls > 0 {
		fmt.Fprintf(&b, "**Unpriced calls:** %d (not in the totals)\n", tot.UnpricedCalls)
	}

	sort.Slice(projects, func(i, j int) bool { return projects[i].Cost > projects[j].Cost })
	if len(projects) > 0 {
		b.WriteString("\n### Top projects\n")
		for i, p := range projects {
			if i == 5 {
				break
			}
			fmt.Fprintf(&b, "- %s — $%.2f (%d sessions)\n", displayProject(p.Project), p.Cost, p.Sessions)
		}
	}
	if len(models) > 0 {
		b.WriteString("\n### Top models\n")
		for i, m := range models {
			if i == 5 {
				break
			}
			fmt.Fprintf(&b, "- %s — $%.2f (%s calls)\n", m.Model, m.Cost, groupInt64(int64(m.Calls)))
		}
	}
	insights := BuildInsightsWithBudgets(sig, tot, burn, o.DailyBudget, o.MonthlyBudget)
	var actionable []Insight
	for _, in := range insights {
		if in.Severity != "info" {
			actionable = append(actionable, in)
		}
	}
	if len(actionable) > 0 {
		b.WriteString("\n### Worth a look\n")
		for _, in := range actionable {
			fmt.Fprintf(&b, "- **%s** — %s\n", in.Headline, in.Detail)
		}
	}
	return b.String(), nil
}

func displayProject(p string) string {
	if p == "" {
		return "(no project)"
	}
	base := filepath.Base(p)
	if base == "." || base == "/" {
		return p
	}
	return base
}

// ---------------------------------------------------------------- notifier

// NotifierConfig configures RunNotifier.
type NotifierConfig struct {
	WebhookURL    string
	DailyBudget   float64
	MonthlyBudget float64
	// Tags resolves tag budgets.
	Tags Tags
	// DigestEvery sends a digest covering the same span on this interval.
	DigestEvery time.Duration
	Log         *slog.Logger
	// Every is how often budgets are checked; default 5 minutes.
	Every  time.Duration
	Client *http.Client
	Now    func() time.Time
}

// RunNotifier checks budgets and sends digests until ctx is done. Each alert
// fires at most once per day (daily budget) or month (monthly budget and
// forecast); the last-sent marks live in the database so a restart does not
// repeat them.
func RunNotifier(ctx context.Context, st *store.Store, cfg NotifierConfig) {
	if cfg.Every <= 0 {
		cfg.Every = 5 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 20 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	ticker := time.NewTicker(cfg.Every)
	defer ticker.Stop()
	for {
		if err := NotifyOnce(ctx, st, cfg); err != nil {
			cfg.Log.Warn("notifier pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// NotifyOnce runs one notifier check.
func NotifyOnce(ctx context.Context, st *store.Store, cfg NotifierConfig) error {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	now := cfg.Now()
	daily, err := st.Daily(ctx, store.Filter{})
	if err != nil {
		return err
	}
	burn := burnWindows(daily, now)
	fc := monthForecast(burn, now)
	dayKey := localDay(now).Format("2006-01-02")
	monthKey := localDay(now).Format("2006-01")

	type alert struct{ key, mark, text string }
	var alerts []alert
	for _, b := range burn {
		if cfg.DailyBudget > 0 && b.Label == "Today" && b.Cost > cfg.DailyBudget {
			alerts = append(alerts, alert{"notify.daily_budget", dayKey,
				fmt.Sprintf("⚠️ dashd: today's spend $%.2f is over the $%.2f daily budget.", b.Cost, cfg.DailyBudget)})
		}
		if cfg.MonthlyBudget > 0 && b.Label == "Month to date" && b.Cost > cfg.MonthlyBudget {
			alerts = append(alerts, alert{"notify.monthly_budget", monthKey,
				fmt.Sprintf("⚠️ dashd: month-to-date spend $%.2f is over the $%.2f monthly budget.", b.Cost, cfg.MonthlyBudget)})
		}
	}
	if cfg.MonthlyBudget > 0 && fc.OK && fc.Projected > cfg.MonthlyBudget {
		alerts = append(alerts, alert{"notify.monthly_forecast", monthKey,
			fmt.Sprintf("📈 dashd: this month is projected at $%.2f (range $%.2f–$%.2f), over the $%.2f budget.",
				fc.Projected, fc.Low, fc.High, cfg.MonthlyBudget)})
	}
	budgets, err := budgetViews(ctx, st, cfg.Tags, now)
	if err != nil {
		return err
	}
	for _, b := range budgets {
		switch b.State {
		case "over":
			alerts = append(alerts, alert{"notify.budget." + b.Scope + "." + b.Name, monthKey + ".over",
				fmt.Sprintf("⚠️ dashd: %s %s has spent %s of its %s monthly budget.", b.Scope, b.Label, b.SpentText, b.MonthlyText)})
		case "forecast":
			// Once a budget has gone over, a later dip back under is not news.
			if prev, _ := st.Meta("notify.budget." + b.Scope + "." + b.Name); prev != monthKey+".over" {
				alerts = append(alerts, alert{"notify.budget." + b.Scope + "." + b.Name, monthKey + ".forecast",
					fmt.Sprintf("📈 dashd: %s %s is projected at %s this month, over its %s budget.", b.Scope, b.Label, b.ProjectedText, b.MonthlyText)})
			}
		}
	}
	for _, a := range alerts {
		if prev, _ := st.Meta(a.key); prev == a.mark {
			continue
		}
		if err := PostWebhook(ctx, cfg.Client, cfg.WebhookURL, a.text); err != nil {
			return err
		}
		_ = st.SetMeta(a.key, a.mark)
		cfg.Log.Info("sent budget alert", "alert", a.key)
	}

	if cfg.DigestEvery > 0 {
		last, _ := st.Meta("notify.digest_at")
		var lastAt time.Time
		if last != "" {
			lastAt, _ = time.Parse(time.RFC3339, last)
		}
		if lastAt.IsZero() || now.Sub(lastAt) >= cfg.DigestEvery {
			days := int(cfg.DigestEvery.Hours() / 24)
			if days < 1 {
				days = 1
			}
			md, err := BuildDigest(ctx, st, DigestOptions{Days: days, DailyBudget: cfg.DailyBudget,
				MonthlyBudget: cfg.MonthlyBudget, Now: now})
			if err != nil {
				return err
			}
			if err := PostWebhook(ctx, cfg.Client, cfg.WebhookURL, md); err != nil {
				return err
			}
			_ = st.SetMeta("notify.digest_at", now.Format(time.RFC3339))
			cfg.Log.Info("sent usage digest", "days", days)
		}
	}
	return nil
}

// PostWebhook sends text as JSON with both "text" (Slack, Mattermost,
// Google Chat) and "content" (Discord) set, which covers the common chat
// webhooks without per-service configuration.
func PostWebhook(ctx context.Context, client *http.Client, url, text string) error {
	if url == "" {
		return fmt.Errorf("no webhook URL configured")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	content := text
	if len(content) > 1900 {
		content = content[:1900] + "…"
	}
	body, _ := json.Marshal(map[string]string{"text": text, "content": content})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dashd")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: status %d", resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------- comparison

// comparisonWindow picks the period the page is about and the equal-length
// period before it: the filter's date range when there is one, otherwise the
// last seven days.
func comparisonWindow(f store.Filter, now time.Time) (curFrom, curTo, priorFrom, priorTo time.Time, label string) {
	curTo = localDay(now).AddDate(0, 0, 1).Add(-time.Second)
	curFrom = localDay(now).AddDate(0, 0, -6)
	label = "Last 7 days"
	if f.DateFrom != nil {
		curFrom = *f.DateFrom
		label = "Selected range"
	}
	if f.DateTo != nil {
		curTo = *f.DateTo
		label = "Selected range"
	}
	span := curTo.Sub(curFrom) + time.Second
	priorTo = curFrom.Add(-time.Second)
	priorFrom = curFrom.Add(-span)
	return
}

// comparisonJSON computes the period comparison shown on the Overview.
func comparisonJSON(ctx context.Context, st *store.Store, f store.Filter, now time.Time) (map[string]any, error) {
	cf, ct, pf, pt, label := comparisonWindow(f, now)
	cur, prior := f, f
	cur.DateFrom, cur.DateTo = &cf, &ct
	prior.DateFrom, prior.DateTo = &pf, &pt
	ctot, err := st.Totals(ctx, cur)
	if err != nil {
		return nil, err
	}
	ptot, err := st.Totals(ctx, prior)
	if err != nil {
		return nil, err
	}
	cm, err := st.Models(ctx, cur)
	if err != nil {
		return nil, err
	}
	pm, err := st.Models(ctx, prior)
	if err != nil {
		return nil, err
	}
	type pair struct{ cur, prior float64 }
	byModel := map[string]*pair{}
	for _, m := range cm {
		byModel[m.Model] = &pair{cur: m.Cost}
	}
	for _, m := range pm {
		if p, ok := byModel[m.Model]; ok {
			p.prior = m.Cost
		} else {
			byModel[m.Model] = &pair{prior: m.Cost}
		}
	}
	models := make([]map[string]any, 0, len(byModel))
	for name, p := range byModel {
		models = append(models, map[string]any{"name": name, "current": p.cur, "prior": p.prior, "delta": p.cur - p.prior})
	}
	sort.Slice(models, func(i, j int) bool {
		a, b := models[i]["delta"].(float64), models[j]["delta"].(float64)
		if a < 0 {
			a = -a
		}
		if b < 0 {
			b = -b
		}
		return a > b
	})
	if len(models) > 8 {
		models = models[:8]
	}
	side := func(t store.Totals) map[string]any {
		return map[string]any{"cost": t.Cost, "calls": t.Calls, "sessions": t.Sessions, "tokens": t.TotalTokens,
			"per_call": ratio(t.Cost, float64(t.Calls))}
	}
	return map[string]any{
		"label":       label,
		"currentFrom": cf.Format("2006-01-02"),
		"currentTo":   ct.Format("2006-01-02"),
		"priorFrom":   pf.Format("2006-01-02"),
		"priorTo":     pt.Format("2006-01-02"),
		"current":     side(ctot),
		"prior":       side(ptot),
		"models":      models,
	}, nil
}
