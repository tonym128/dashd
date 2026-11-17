package web

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
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
}

// BuildInsights turns pre-aggregated store signals, totals, and burn windows
// into ordered, readable insights. Both the web UI and `dashd insights` CLI
// use this same generator.
func BuildInsights(sig store.InsightSignals, totals store.Totals, burn []burnWindow) []Insight {
	if totals.Calls == 0 {
		return nil
	}
	var out []Insight

	// 1. Unpriced calls warning (highest priority data-quality signal).
	if totals.UnpricedCalls > 0 {
		top := ""
		if len(sig.Unpriced) > 0 {
			top = fmt.Sprintf(" Top unpriced: %s (%d calls).", sig.Unpriced[0].Model, sig.Unpriced[0].Calls)
		}
		out = append(out, Insight{
			ID:       "unpriced-calls",
			Category: "Data Health",
			Severity: "warn",
			Headline: fmt.Sprintf("%s unpriced calls are excluded from cost totals", groupInt64(int64(totals.UnpricedCalls))),
			Detail:   "These calls count tokens but show $0.00 until a rate is known." + top + " Run `dashd update-models` and `dashd reprice`.",
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
				ID:       "burn-pace",
				Category: "Cost & Burn",
				Severity: sev,
				Headline: fmt.Sprintf("7-day pace is $%.2f/day (~$%.0f/30d)", dailyAvg, projMonth),
				Detail:   fmt.Sprintf("Spent $%.2f over the last %d days%s.", b.Cost, b.Days, deltaNote),
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
			ID:       "retry-loops",
			Category: "Tools & Loops",
			Severity: "warn",
			Headline: fmt.Sprintf("%d high-churn session(s) with ≥20%% tool error rate", sig.HighChurnSessions),
			Detail:   fmt.Sprintf("Sessions with ≥40 calls and heavy tool failures cost $%.2f — often caused by repeated failing test or build loops.", sig.HighChurnCost),
		})
	}

	// 5. Tool error hotspot.
	if sig.ErrorToolName != "" && sig.ErrorToolCalls > 0 {
		errPct := float64(sig.ErrorToolErrors) / float64(sig.ErrorToolCalls) * 100
		sev := "tip"
		if errPct >= 15 {
			sev = "warn"
		}
		out = append(out, Insight{
			ID:       "tool-errors",
			Category: "Tools & Loops",
			Severity: sev,
			Headline: fmt.Sprintf("%s failed %.1f%% of the time (%d/%d calls)", sig.ErrorToolName, errPct, sig.ErrorToolErrors, sig.ErrorToolCalls),
			Detail:   fmt.Sprintf("Tool failures in %s are associated with $%.2f in follow-up LLM spend.", sig.ErrorToolName, sig.ErrorToolCost),
		})
	}

	// 6. Prompt caching hit rate & low-cache project warning.
	cacheDenom := totals.InputTokens + totals.CacheReadTokens + totals.CacheWriteTokens
	if cacheDenom > 0 && totals.CacheReadTokens > 0 {
		hitPct := float64(totals.CacheReadTokens) / float64(cacheDenom) * 100
		out = append(out, Insight{
			ID:       "cache-efficiency",
			Category: "Caching",
			Severity: "info",
			Headline: fmt.Sprintf("Overall prompt cache hit rate is %.1f%% (%s cached tokens)", hitPct, formatCompact(totals.CacheReadTokens)),
			Detail:   fmt.Sprintf("Cache reads offset %s out of %s total prompt tokens.", formatCompact(totals.CacheReadTokens), formatCompact(cacheDenom)),
		})
	}
	if sig.LowCacheProject != "" && sig.LowCacheProjectHit < 25 {
		projName := filepath.Base(sig.LowCacheProject)
		if projName == "" || projName == "." || projName == "/" {
			projName = sig.LowCacheProject
		}
		out = append(out, Insight{
			ID:          "low-cache-project",
			Category:    "Caching",
			Severity:    "tip",
			Headline:    fmt.Sprintf("Low cache hit rate (%.1f%%) in %s", sig.LowCacheProjectHit, projName),
			Detail:      fmt.Sprintf("Across %d calls ($%.2f), %s reused little cached context — shorter restarts or dynamic system prompts can increase input cost.", sig.LowCacheProjectCall, sig.LowCacheProjectCost, projName),
			ActionLabel: "Filter project",
			ActionURL:   "/?project=" + url.QueryEscape(sig.LowCacheProject),
		})
	}

	// 7. Long-prompt tier hits (>= 200K context).
	if sig.LongPromptCalls > 0 {
		out = append(out, Insight{
			ID:       "long-prompt-tier",
			Category: "Model Efficiency",
			Severity: "tip",
			Headline: fmt.Sprintf("%s calls crossed 200K+ context tokens ($%.2f)", groupInt64(int64(sig.LongPromptCalls)), sig.LongPromptCost),
			Detail:   "Long-context prompts trigger higher per-token pricing tiers on Pro/flagship models; compacting or starting fresh sessions earlier reduces tier-2 charges.",
		})
	}

	// 8. Model cost concentration & reasoning token share.
	if sig.TopModelName != "" && sig.TopModelPct >= 45 {
		out = append(out, Insight{
			ID:          "top-model",
			Category:    "Model Efficiency",
			Severity:    "info",
			Headline:    fmt.Sprintf("%s drives %.0f%% of total spend ($%.2f)", sig.TopModelName, sig.TopModelPct, sig.TopModelCost),
			Detail:      "Routing routine or exploratory tasks to a Flash/Sonnet-tier model can cut cost without slowing iteration.",
			ActionLabel: "Filter model",
			ActionURL:   "/?model=" + url.QueryEscape(sig.TopModelName),
		})
	}
	genTotal := sig.OutputTokens + sig.ReasoningTokens
	if genTotal > 0 && sig.ReasoningTokens > 0 {
		rPct := float64(sig.ReasoningTokens) / float64(genTotal) * 100
		if rPct >= 25 {
			out = append(out, Insight{
				ID:       "reasoning-share",
				Category: "Model Efficiency",
				Severity: "info",
				Headline: fmt.Sprintf("%.0f%% of generated tokens are reasoning/thinking tokens", rPct),
				Detail:   fmt.Sprintf("%s reasoning tokens vs. %s visible output tokens (both are billed at the output rate).", formatCompact(sig.ReasoningTokens), formatCompact(sig.OutputTokens)),
			})
		}
	}

	// 9. Time split (LLM wait vs Tool execution) & slowest tool.
	activeTime := totals.LLMSeconds + totals.ToolSeconds
	if activeTime > 60 && sig.TopToolName != "" {
		toolPct := totals.ToolSeconds / activeTime * 100
		out = append(out, Insight{
			ID:       "time-split",
			Category: "Tools & Loops",
			Severity: "info",
			Headline: fmt.Sprintf("Tools account for %.0f%% of active wait time (%s)", toolPct, humanDuration(totals.ToolSeconds)),
			Detail:   fmt.Sprintf("Slowest tool: %s (%s across %s calls), compared to %s waiting on LLMs.", sig.TopToolName, humanDuration(sig.TopToolSeconds), groupInt64(int64(sig.TopToolCalls)), humanDuration(totals.LLMSeconds)),
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
			Category:    "Machines & Health",
			Severity:    "info",
			Headline:    fmt.Sprintf("%s accounts for %.0f%% of spend across %d machines", sig.TopMachine, sig.TopMachinePct, sig.MachineCount),
			Detail:      fmt.Sprintf("%s logged $%.2f.%s", sig.TopMachine, sig.TopMachineCost, speedNote),
			ActionLabel: "Filter machine",
			ActionURL:   "/?machine=" + url.QueryEscape(sig.TopMachine),
		})
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
	return BuildInsights(sig, totals, burn), nil
}
