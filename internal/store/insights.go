package store

import (
	"context"
	"database/sql"
	"fmt"
)

// InsightSignals holds the pre-aggregated signals used to generate human-readable
// cost, efficiency, caching, tooling, and multi-machine insights.
type InsightSignals struct {
	SessionCount      int
	MedianSessionCost float64
	TopSession        SessionRow
	Top3SessionsCost  float64
	HighChurnSessions int
	HighChurnCost     float64

	LongPromptCalls int
	LongPromptCost  float64

	TopModelName string
	TopModelCost float64
	TopModelPct  float64

	ReasoningTokens int64
	OutputTokens    int64

	LowCacheProject     string
	LowCacheProjectHit  float64
	LowCacheProjectCost float64
	LowCacheProjectCall int

	TopToolName     string
	TopToolCalls    int
	TopToolSeconds  float64
	ErrorToolName   string
	ErrorToolCalls  int
	ErrorToolErrors int
	ErrorToolCost   float64

	MachineCount   int
	TopMachine     string
	TopMachineCost float64
	TopMachinePct  float64
	FastMachine    string
	FastMachineTPS float64

	Unpriced []UnpricedModel
}

// InsightSignals computes all insight inputs under Filter in one pass.
func (s *Store) InsightSignals(ctx context.Context, f Filter, totals Totals) (InsightSignals, error) {
	var sig InsightSignals

	// 1. Session outliers & high-churn retry loops.
	sessWhere, sessArgs := f.whereSession("")
	var count int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM session`+sessWhere, sessArgs...).Scan(&count); err != nil {
		return sig, fmt.Errorf("insight session count: %w", err)
	}
	sig.SessionCount = count
	if count > 0 {
		medArgs := append(append([]any{}, sessArgs...), count/2)
		_ = s.q.QueryRowContext(ctx,
			`SELECT cost_usd FROM session`+sessWhere+` ORDER BY cost_usd ASC LIMIT 1 OFFSET ?`,
			medArgs...).Scan(&sig.MedianSessionCost)

		topRows, err := s.q.QueryContext(ctx,
			`SELECT `+sessionColumns+` FROM session`+sessWhere+` ORDER BY cost_usd DESC LIMIT 3`,
			sessArgs...)
		if err != nil {
			return sig, fmt.Errorf("insight top sessions: %w", err)
		}
		for i := 0; topRows.Next(); i++ {
			r, scanErr := scanSessionRow(topRows)
			if scanErr != nil {
				topRows.Close()
				return sig, scanErr
			}
			if i == 0 {
				sig.TopSession = r
			}
			sig.Top3SessionsCost += r.Cost
		}
		topRows.Close()

		churnWhere, churnArgs := f.whereSession("")
		extra := "calls >= 40 AND tool_calls >= 10 AND (tool_errors * 1.0 / tool_calls) >= 0.20"
		if churnWhere == "" {
			churnWhere = " WHERE " + extra
		} else {
			churnWhere += " AND " + extra
		}
		_ = s.q.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(cost_usd), 0) FROM session`+churnWhere,
			churnArgs...).Scan(&sig.HighChurnSessions, &sig.HighChurnCost)
	}

	// 2. Long-prompt tier hits (>= 200K context tokens) and reasoning share.
	callWhere, callArgs := f.where("", "ts")
	tierWhere, tierArgs := f.whereExtra("", "ts", "(input_tokens + cache_read_tokens + cache_write_tokens) >= 200000")
	_ = s.q.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cost_usd), 0) FROM call`+tierWhere,
		tierArgs...).Scan(&sig.LongPromptCalls, &sig.LongPromptCost)

	sig.ReasoningTokens = totals.ReasoningTokens
	sig.OutputTokens = totals.OutputTokens

	// 3. Top model by cost.
	if err := s.q.QueryRowContext(ctx,
		`SELECT model, COALESCE(SUM(cost_usd), 0) FROM call`+callWhere+` GROUP BY model ORDER BY SUM(cost_usd) DESC LIMIT 1`,
		callArgs...).Scan(&sig.TopModelName, &sig.TopModelCost); err != nil && err != sql.ErrNoRows {
		return sig, fmt.Errorf("insight top model: %w", err)
	}
	if totals.Cost > 0 {
		sig.TopModelPct = sig.TopModelCost / totals.Cost * 100
	}

	// 4. Lowest cache-hit project with meaningful volume (>= 20 calls and > $0.50).
	_ = s.q.QueryRowContext(ctx, `
		SELECT project,
		       COUNT(*) AS c,
		       COALESCE(SUM(cost_usd), 0) AS cost,
		       CASE WHEN SUM(input_tokens + cache_read_tokens + cache_write_tokens) > 0
		            THEN SUM(cache_read_tokens) * 100.0 / SUM(input_tokens + cache_read_tokens + cache_write_tokens)
		            ELSE 0 END AS hit_pct
		FROM call`+callWhere+`
		GROUP BY project
		HAVING c >= 20 AND cost >= 0.50 AND project != ''
		ORDER BY hit_pct ASC, cost DESC
		LIMIT 1`, callArgs...).Scan(&sig.LowCacheProject, &sig.LowCacheProjectCall, &sig.LowCacheProjectCost, &sig.LowCacheProjectHit)

	// 5. Tools: slowest/most-used and highest-error-rate tool.
	tools, err := s.Tools(ctx, f)
	if err != nil {
		return sig, err
	}
	if len(tools) > 0 {
		sig.TopToolName = tools[0].Tool
		sig.TopToolCalls = tools[0].Calls
		sig.TopToolSeconds = tools[0].Seconds
		bestRate := 0.0
		for _, t := range tools {
			if t.Calls >= 10 && t.Errors >= 3 {
				rate := float64(t.Errors) / float64(t.Calls)
				if rate > bestRate {
					bestRate = rate
					sig.ErrorToolName = t.Tool
					sig.ErrorToolCalls = t.Calls
					sig.ErrorToolErrors = t.Errors
					sig.ErrorToolCost = t.Cost
				}
			}
		}
	}

	// 6. Machines breakdown.
	machines, err := s.MachinesBreakdown(ctx, f)
	if err != nil {
		return sig, err
	}
	sig.MachineCount = len(machines)
	if len(machines) > 0 {
		sig.TopMachine = machines[0].Machine
		sig.TopMachineCost = machines[0].Cost
		if totals.Cost > 0 {
			sig.TopMachinePct = machines[0].Cost / totals.Cost * 100
		}
		for _, m := range machines {
			if m.LLMSeconds > 10 {
				tps := float64(m.OutputTokens) / m.LLMSeconds
				if tps > sig.FastMachineTPS {
					sig.FastMachineTPS = tps
					sig.FastMachine = m.Machine
				}
			}
		}
	}

	// 7. Unpriced models.
	if totals.UnpricedCalls > 0 {
		sig.Unpriced, _ = s.UnpricedModels(ctx)
	}

	return sig, nil
}
