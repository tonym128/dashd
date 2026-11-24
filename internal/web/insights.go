package web

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tonym128/dashd/internal/store"
)

// Insight is one human-readable takeaway derived from the stored usage data.
type Insight struct {
	ID          string `json:"id"`
	Category    string `json:"category"`
	Severity    string `json:"severity"` // "warn", "tip", "info"
	Headline    string `json:"headline"`
	Detail      string `json:"detail"`
	ActionLabel string `json:"action_label,omitempty"`
	ActionURL   string `json:"action_url,omitempty"`
	// Impact is the dollars the insight is about, which orders it within its
	// severity.
	Impact float64 `json:"impact_usd,omitempty"`
}

// BuildInsights turns pre-aggregated store signals, totals, and burn windows
// into ordered, readable insights. Both the web UI and `dashd insights` CLI
// use this same generator.
func BuildInsights(sig store.InsightSignals, totals store.Totals, burn []burnWindow) []Insight {
	return BuildInsightsWithBudgets(sig, totals, burn, 0, 0)
}

// BuildInsightsWithBudgets behaves like BuildInsights and also flags daily or
// monthly budget overruns when positive budgets are configured.
func BuildInsightsWithBudgets(sig store.InsightSignals, totals store.Totals, burn []burnWindow, dailyBudget, monthlyBudget float64) []Insight {
	return BuildInsightsFull(InsightInputs{Signals: sig, Totals: totals, Burn: burn,
		DailyBudget: dailyBudget, MonthlyBudget: monthlyBudget})
}

// InsightInputs is everything the insight engine reads.
type InsightInputs struct {
	Signals       store.InsightSignals
	Totals        store.Totals
	Burn          []burnWindow
	Forecast      forecast
	DailyBudget   float64
	MonthlyBudget float64
	CacheSavings  float64
	Machines      []machineStatusView
	// PriceError is why the last price refresh failed, if it did.
	PriceError string
	// Query is the page's current filter, kept on action links so following
	// an insight narrows the view rather than resetting it.
	Query url.Values
	Now   time.Time
}

// withQuery merges extra parameters into the current filter query.
func withQuery(base url.Values, extra map[string]string) string {
	q := url.Values{}
	for k, v := range base {
		if k == "view" || k == "sort" || k == "asc" {
			continue
		}
		q[k] = append([]string(nil), v...)
	}
	for k, v := range extra {
		q.Set(k, v)
	}
	return "/?" + q.Encode()
}

// BuildInsightsFull is the insight engine.
func BuildInsightsFull(in InsightInputs) []Insight {
	sig, totals, burn := in.Signals, in.Totals, in.Burn
	dailyBudget, monthlyBudget := in.DailyBudget, in.MonthlyBudget
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	var out []Insight

	// Machines whose push agent reports a failure, or that went quiet, come
	// first: their data is missing from everything below.
	var failing []machineStatusView
	for _, m := range in.Machines {
		if m.LastError != "" {
			failing = append(failing, m)
		}
	}
	if len(failing) > 1 {
		names := make([]string, len(failing))
		for i, m := range failing {
			names[i] = m.Machine
		}
		out = append(out, Insight{
			ID:          "machines-failing",
			Category:    "Machines & Health",
			Severity:    "warn",
			Headline:    fmt.Sprintf("%d machines are failing to push", len(failing)),
			Detail:      strings.Join(names, ", ") + ". Their newest sessions are missing from these totals; run `dashd status` on each.",
			ActionLabel: "Open System tab",
			ActionURL:   withQuery(in.Query, map[string]string{"view": "system"}),
		})
		failing = nil
	}
	for _, m := range failing {
		out = append(out, Insight{
			ID:          "machine-failing-" + m.Machine,
			Category:    "Machines & Health",
			Severity:    "warn",
			Headline:    fmt.Sprintf("%s is failing to push (%s)", m.Machine, m.LastErrorAgo),
			Detail:      "Last error: " + m.LastError + ". Its newest sessions are missing from these totals; run `dashd status` on that machine.",
			ActionLabel: "Open System tab",
			ActionURL:   withQuery(in.Query, map[string]string{"view": "system"}),
		})
	}
	if in.PriceError != "" {
		out = append(out, Insight{
			ID:          "price-refresh-failing",
			Category:    "Data Health",
			Severity:    "warn",
			Headline:    "The price list is not refreshing",
			Detail:      "Last error: " + in.PriceError + ". New models will show as unpriced and price changes will be missed.",
			ActionLabel: "Open System tab",
			ActionURL:   withQuery(in.Query, map[string]string{"view": "system"}),
		})
	}
	if totals.Calls == 0 {
		return out
	}

	// 0. Budget threshold alerts (when configured).
	for _, b := range burn {
		if dailyBudget > 0 && b.Label == "Today" && b.Cost > dailyBudget {
			out = append(out, Insight{
				ID:       "daily-budget-over",
				Impact:   b.Cost,
				Category: "Cost & Burn",
				Severity: "warn",
				Headline: fmt.Sprintf("Today's spend ($%.2f) exceeds your $%.2f daily budget", b.Cost, dailyBudget),
				Detail:   fmt.Sprintf("Currently %.0f%% of your configured daily soft budget.", b.Cost/dailyBudget*100),
			})
		}
		if monthlyBudget > 0 && b.Label == "Month to date" && b.Cost > monthlyBudget {
			out = append(out, Insight{
				ID:       "monthly-budget-over",
				Impact:   b.Cost,
				Category: "Cost & Burn",
				Severity: "warn",
				Headline: fmt.Sprintf("Month-to-date spend ($%.2f) exceeds your $%.2f monthly budget", b.Cost, monthlyBudget),
				Detail:   fmt.Sprintf("Currently %.0f%% of your configured monthly soft budget.", b.Cost/monthlyBudget*100),
			})
		}
	}

	if monthlyBudget > 0 && in.Forecast.OK && in.Forecast.Projected > monthlyBudget {
		var overNow bool
		for _, b := range burn {
			overNow = overNow || (b.Label == "Month to date" && b.Cost > monthlyBudget)
		}
		if !overNow {
			out = append(out, Insight{
				ID:       "monthly-forecast-over",
				Impact:   in.Forecast.Projected,
				Category: "Cost & Burn",
				Severity: "warn",
				Headline: fmt.Sprintf("This month is on track for $%.2f, over your $%.2f budget", in.Forecast.Projected, monthlyBudget),
				Detail: fmt.Sprintf("Projected range $%.2f–$%.2f with %d days left; $%.2f spent so far.",
					in.Forecast.Low, in.Forecast.High, in.Forecast.DaysLeft, in.Forecast.MTD),
				ActionLabel: "Show this month",
				ActionURL: withQuery(in.Query, map[string]string{
					"date_from": time.Date(in.Now.Year(), in.Now.Month(), 1, 0, 0, 0, 0, time.Local).Format("2006-01-02")}),
			})
		}
	}

	// 1. Unpriced calls warning (highest priority data-quality signal).
	if totals.UnpricedCalls > 0 {
		top := ""
		unpricedSeverity := "warn"
		if float64(totals.UnpricedCalls) < 0.01*float64(totals.Calls) {
			unpricedSeverity = "tip"
		}
		if len(sig.Unpriced) > 0 {
			top = fmt.Sprintf(" Top unpriced: %s (%d calls).", sig.Unpriced[0].Model, sig.Unpriced[0].Calls)
		}
		out = append(out, Insight{
			ID:          "unpriced-calls",
			Category:    "Data Health",
			Severity:    unpricedSeverity,
			Headline:    fmt.Sprintf("%s unpriced calls are excluded from cost totals", groupInt64(int64(totals.UnpricedCalls))),
			Detail:      "These calls count tokens but show $0.00 until a rate is known." + top + " Run `dashd update-models` and `dashd reprice`.",
			ActionLabel: "Show unpriced calls",
			ActionURL:   withQuery(in.Query, map[string]string{"unpriced": "1"}),
		})
	}

	// 2. Burn velocity & monthly projection (from Last 7 days & MTD burn windows).
	for _, b := range burn {
		if b.Label == "Last 7 days" && b.Days > 0 && b.Cost > 0 {
			dailyAvg := b.Cost / float64(b.Days)
			projMonth := dailyAvg * 30
			sev := "info"
			deltaNote := ""
			if b.PriorCost > 0 {
				pct := (b.Cost - b.PriorCost) / b.PriorCost * 100
				if pct >= 25 {
					sev = "warn"
					deltaNote = fmt.Sprintf(" (up %.0f%% vs. the prior 7 days)", pct)
				} else if pct <= -20 {
					deltaNote = fmt.Sprintf(" (down %.0f%% vs. the prior 7 days)", -pct)
				}
			}
			out = append(out, Insight{
				ID:          "burn-pace",
				Impact:      b.Cost,
				Category:    "Cost & Burn",
				Severity:    sev,
				Headline:    fmt.Sprintf("7-day pace is $%.2f/day (~$%.0f/30d)", dailyAvg, projMonth),
				Detail:      fmt.Sprintf("Spent $%.2f over the last %d days%s.", b.Cost, b.Days, deltaNote),
				ActionLabel: "Show last 7 days",
				ActionURL: withQuery(in.Query, map[string]string{
					"date_from": localDay(in.Now).AddDate(0, 0, -6).Format("2006-01-02")}),
			})
			break
		}
	}

	// 3. Session cost concentration & outlier.
	if sig.SessionCount >= 3 && totals.Cost > 0 && sig.TopSession.UID != "" {
		top3Pct := sig.Top3SessionsCost / totals.Cost * 100
		projName := filepath.Base(sig.TopSession.Project)
		if projName == "" || projName == "." || projName == "/" {
			projName = sig.TopSession.Project
		}
		multNote := ""
		if sig.MedianSessionCost > 0.01 && sig.TopSession.Cost >= sig.MedianSessionCost*3 {
			multNote = fmt.Sprintf(" (%.1f× your $%.2f median session)", sig.TopSession.Cost/sig.MedianSessionCost, sig.MedianSessionCost)
		}
		out = append(out, Insight{
			ID:          "session-outlier",
			Impact:      sig.Top3SessionsCost,
			Category:    "Cost & Burn",
			Severity:    "info",
			Headline:    fmt.Sprintf("Top 3 sessions account for %.0f%% of total spend ($%.2f)", top3Pct, sig.Top3SessionsCost),
			Detail:      fmt.Sprintf("Most expensive session: %s ($%.2f across %d calls)%s.", projName, sig.TopSession.Cost, sig.TopSession.Calls, multNote),
			ActionLabel: "Inspect top session",
			ActionURL:   "/session?uid=" + url.QueryEscape(sig.TopSession.UID),
		})
	}

	// 4. High-churn / retry loop detection.
	if sig.HighChurnSessions > 0 {
		out = append(out, Insight{
			ID:          "retry-loops",
			Impact:      sig.HighChurnCost,
			Category:    "Tools & Loops",
			Severity:    "warn",
			Headline:    fmt.Sprintf("%d high-churn session(s) with ≥20%% tool error rate", sig.HighChurnSessions),
			Detail:      fmt.Sprintf("Sessions with ≥40 calls and heavy tool failures cost $%.2f — often caused by repeated failing test or build loops.", sig.HighChurnCost),
			ActionLabel: "Sessions by tool errors",
			ActionURL:   withQuery(in.Query, map[string]string{"view": "sessions", "sort": "errors"}),
		})
	}

	// 5. Tool error hotspot.
	if sig.ErrorToolName != "" && sig.ErrorToolCalls > 0 {
		errPct := float64(sig.ErrorToolErrors) / float64(sig.ErrorToolCalls) * 100
		sev := "tip"
		// A handful of failures worth pocket change is not a warning.
		if errPct >= 15 && sig.ErrorToolCost >= 5 {
			sev = "warn"
		}
		out = append(out, Insight{
			ID:          "tool-errors",
			Impact:      sig.ErrorToolCost,
			Category:    "Tools & Loops",
			Severity:    sev,
			Headline:    fmt.Sprintf("%s failed %.1f%% of the time (%d/%d calls)", sig.ErrorToolName, errPct, sig.ErrorToolErrors, sig.ErrorToolCalls),
			Detail:      fmt.Sprintf("Tool failures in %s are associated with $%.2f in follow-up LLM spend.", sig.ErrorToolName, sig.ErrorToolCost),
			ActionLabel: "Open tools breakdown",
			ActionURL:   withQuery(in.Query, map[string]string{"view": "breakdown", "sub": "tools"}),
		})
	}

	// 6. Prompt caching: what it saved (the hit rate alone is already on the
	// Total Tokens card), and the project that uses it least.
	cacheDenom := totals.InputTokens + totals.CacheReadTokens + totals.CacheWriteTokens
	if in.CacheSavings >= 0.01 && cacheDenom > 0 {
		hitPct := float64(totals.CacheReadTokens) / float64(cacheDenom) * 100
		out = append(out, Insight{
			ID:       "cache-savings",
			Impact:   in.CacheSavings,
			Category: "Caching",
			Severity: "info",
			Headline: fmt.Sprintf("Prompt caching saved about $%.2f (%.1f%% hit rate)", in.CacheSavings, hitPct),
			Detail: fmt.Sprintf("%s cached prompt tokens were billed at the cache-read rate instead of the input rate; without caching this view would cost ~$%.2f.",
				formatCompact(totals.CacheReadTokens), totals.Cost+in.CacheSavings),
		})
	}
	if sig.LowCacheProject != "" && sig.LowCacheProjectHit < 25 {
		projName := filepath.Base(sig.LowCacheProject)
		if projName == "" || projName == "." || projName == "/" {
			projName = sig.LowCacheProject
		}
		out = append(out, Insight{
			ID:          "low-cache-project",
			Impact:      sig.LowCacheProjectCost,
			Category:    "Caching",
			Severity:    "tip",
			Headline:    fmt.Sprintf("Low cache hit rate (%.1f%%) in %s", sig.LowCacheProjectHit, projName),
			Detail:      fmt.Sprintf("Across %d calls ($%.2f), %s reused little cached context — shorter restarts or dynamic system prompts can increase input cost.", sig.LowCacheProjectCall, sig.LowCacheProjectCost, projName),
			ActionLabel: "Filter project",
			ActionURL:   withQuery(in.Query, map[string]string{"project": sig.LowCacheProject}),
		})
	}

	// 7. Long-prompt tier hits (>= 200K context).
	if sig.LongPromptCalls > 0 {
		out = append(out, Insight{
			ID:          "long-prompt-tier",
			Impact:      sig.LongPromptCost,
			Category:    "Model Efficiency",
			Severity:    "tip",
			Headline:    fmt.Sprintf("%s calls crossed 200K+ context tokens ($%.2f)", groupInt64(int64(sig.LongPromptCalls)), sig.LongPromptCost),
			Detail:      "Long-context prompts trigger higher per-token pricing tiers on Pro/flagship models; compacting or starting fresh sessions earlier reduces tier-2 charges.",
			ActionLabel: "Sessions by tokens",
			ActionURL:   withQuery(in.Query, map[string]string{"view": "sessions", "sort": "tokens"}),
		})
	}

	// 8. Model cost concentration & reasoning token share.
	if sig.TopModelName != "" && sig.TopModelPct >= 45 {
		out = append(out, Insight{
			ID:          "top-model",
			Impact:      sig.TopModelCost,
			Category:    "Model Efficiency",
			Severity:    "info",
			Headline:    fmt.Sprintf("%s drives %.0f%% of total spend ($%.2f)", sig.TopModelName, sig.TopModelPct, sig.TopModelCost),
			Detail:      "Routing routine or exploratory tasks to a Flash/Sonnet-tier model can cut cost without slowing iteration.",
			ActionLabel: "Filter model",
			ActionURL:   withQuery(in.Query, map[string]string{"model": sig.TopModelName}),
		})
	}
	genTotal := sig.OutputTokens + sig.ReasoningTokens
	if genTotal > 0 && sig.ReasoningTokens > 0 {
		rPct := float64(sig.ReasoningTokens) / float64(genTotal) * 100
		if rPct >= 25 {
			out = append(out, Insight{
				ID:          "reasoning-share",
				Category:    "Model Efficiency",
				Severity:    "info",
				Headline:    fmt.Sprintf("%.0f%% of generated tokens are reasoning/thinking tokens", rPct),
				Detail:      fmt.Sprintf("%s reasoning tokens vs. %s visible output tokens (both are billed at the output rate).", formatCompact(sig.ReasoningTokens), formatCompact(sig.OutputTokens)),
				ActionLabel: "Compare models",
				ActionURL:   withQuery(in.Query, map[string]string{"view": "breakdown", "sub": "models"}),
			})
		}
	}

	// 9. Time split (LLM wait vs Tool execution) & slowest tool.
	activeTime := totals.LLMSeconds + totals.ToolSeconds
	if activeTime > 60 && sig.TopToolName != "" {
		toolPct := totals.ToolSeconds / activeTime * 100
		out = append(out, Insight{
			ID:          "time-split",
			Category:    "Tools & Loops",
			Severity:    "info",
			Headline:    fmt.Sprintf("Tools account for %.0f%% of active wait time (%s)", toolPct, humanDuration(totals.ToolSeconds)),
			Detail:      fmt.Sprintf("Slowest tool: %s (%s across %s calls), compared to %s waiting on LLMs.", sig.TopToolName, humanDuration(sig.TopToolSeconds), groupInt64(int64(sig.TopToolCalls)), humanDuration(totals.LLMSeconds)),
			ActionLabel: "Open tools breakdown",
			ActionURL:   withQuery(in.Query, map[string]string{"view": "breakdown", "sub": "tools"}),
		})
	}

	// 10. Multi-machine split.
	if sig.MachineCount > 1 && sig.TopMachine != "" {
		speedNote := ""
		if sig.FastMachine != "" {
			speedNote = fmt.Sprintf(" Fastest average throughput: %s (%.1f tok/s).", sig.FastMachine, sig.FastMachineTPS)
		}
		out = append(out, Insight{
			ID:          "multi-machine",
			Impact:      sig.TopMachineCost,
			Category:    "Machines & Health",
			Severity:    "info",
			Headline:    fmt.Sprintf("%s accounts for %.0f%% of spend across %d machines", sig.TopMachine, sig.TopMachinePct, sig.MachineCount),
			Detail:      fmt.Sprintf("%s logged $%.2f.%s", sig.TopMachine, sig.TopMachineCost, speedNote),
			ActionLabel: "Filter machine",
			ActionURL:   withQuery(in.Query, map[string]string{"machine": sig.TopMachine}),
		})
	}

	sevRank := map[string]int{"warn": 0, "tip": 1, "info": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := sevRank[out[i].Severity], sevRank[out[j].Severity]; a != b {
			return a < b
		}
		return out[i].Impact > out[j].Impact
	})
	for i := range out {
		out[i].Headline = groupDollars(out[i].Headline)
		out[i].Detail = groupDollars(out[i].Detail)
	}
	return out
}

// BuildInsightsFromStore computes insights directly from a Store for CLI output.
func BuildInsightsFromStore(ctx context.Context, st *store.Store, f store.Filter, now time.Time) ([]Insight, error) {
	var (
		totals store.Totals
		daily  []store.DayBucket
		sig    store.InsightSignals
	)
	if err := st.ReadTx(ctx, func(tx *store.Store) error {
		var err error
		if totals, err = tx.Totals(ctx, f); err != nil {
			return err
		}
		if daily, err = tx.Daily(ctx, f); err != nil {
			return err
		}
		sig, err = tx.InsightSignals(ctx, f, totals)
		return err
	}); err != nil {
		return nil, err
	}
	burn := burnWindows(daily, now)
	return BuildInsightsFull(InsightInputs{Signals: sig, Totals: totals, Burn: burn,
		Forecast: monthForecast(burn, now), Now: now}), nil
}
