package web

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tonym128/dashd/internal/store"
)

// resumeUID strips the "<machine>::" namespace the server adds when two
// machines push the same session id; the agent only knows the original id.
func resumeUID(uid, machine string) string {
	if machine != "" {
		return strings.TrimPrefix(uid, machine+"::")
	}
	return uid
}

func shQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// resumeCommand mirrors buildResumeCmd in dashboard.js: every argument is
// single-quoted for sh, because the page offers the result as a command.
func resumeCommand(s store.SessionRow) string {
	cwd := shQuote(s.Project)
	uid := shQuote(resumeUID(s.UID, s.Machine))
	switch s.Agent {
	case "claude", "codex":
		return "cd " + cwd + " && " + shQuote(s.Agent) + " --resume " + uid
	case "agy":
		return "cd " + cwd + " && " + shQuote("agy") + " --conversation " + uid
	case "opencode":
		return "cd " + cwd + " && " + shQuote("opencode") + " --session " + uid
	}
	return "cd " + cwd + " && " + shQuote(s.Agent) + " --session " + shQuote(strings.ReplaceAll(s.Path, `\`, "/"))
}

type modelSplitRow struct {
	Model  string
	Calls  int
	Tokens string
	Cost   string
	Pct    string
	Width  string
}

// sessionModelSplit is the per-model cost breakdown inside one session.
func sessionModelSplit(calls []store.CallRow, total float64) []modelSplitRow {
	type agg struct {
		calls  int
		tokens int64
		cost   float64
	}
	by := map[string]*agg{}
	for _, c := range calls {
		a := by[c.Model]
		if a == nil {
			a = &agg{}
			by[c.Model] = a
		}
		a.calls++
		a.tokens += c.TotalTokens
		a.cost += c.Cost
	}
	names := make([]string, 0, len(by))
	for k := range by {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return by[names[i]].cost > by[names[j]].cost })
	out := make([]modelSplitRow, 0, len(names))
	for _, n := range names {
		a := by[n]
		pct := 0.0
		if total > 0 {
			pct = a.cost / total * 100
		}
		out = append(out, modelSplitRow{Model: n, Calls: a.calls, Tokens: formatCompact(a.tokens),
			Cost: fmt.Sprintf("$%.4f", a.cost), Pct: fmt.Sprintf("%.1f%%", pct), Width: fmt.Sprintf("%.1f%%", pct)})
	}
	return out
}

type toolView struct {
	Time    string
	Tool    string
	Seconds string
	IsError bool
}

func sessionToolViews(tools []store.SessionTool) []toolView {
	out := make([]toolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolView{Time: formatUnix(t.TS), Tool: t.Tool, Seconds: humanDuration(t.Seconds), IsError: t.IsError})
	}
	return out
}

type toolSummaryRow struct {
	Tool   string
	Calls  int
	Errors int
	Time   string
}

func sessionToolSummary(tools []store.SessionTool) []toolSummaryRow {
	type agg struct {
		calls, errors int
		secs          float64
	}
	by := map[string]*agg{}
	for _, t := range tools {
		name := t.Tool
		if name == "" {
			name = "(unnamed)"
		}
		a := by[name]
		if a == nil {
			a = &agg{}
			by[name] = a
		}
		a.calls++
		a.secs += t.Seconds
		if t.IsError {
			a.errors++
		}
	}
	out := make([]toolSummaryRow, 0, len(by))
	for k, a := range by {
		out = append(out, toolSummaryRow{Tool: k, Calls: a.calls, Errors: a.errors, Time: humanDuration(a.secs)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Calls > out[j].Calls })
	return out
}

// sparkView is a cumulative-cost line as SVG polyline points in a 300x60 box.
type sparkView struct {
	Points string
	Max    string
	OK     bool
}

func sessionSpark(calls []store.CallRow) sparkView {
	var dated []store.CallRow
	for _, c := range calls {
		if c.TS > 0 {
			dated = append(dated, c)
		}
	}
	if len(dated) < 2 {
		return sparkView{}
	}
	t0, t1 := dated[0].TS, dated[len(dated)-1].TS
	var total float64
	for _, c := range dated {
		total += c.Cost
	}
	if t1 <= t0 || total <= 0 {
		return sparkView{}
	}
	var b strings.Builder
	var cum float64
	for i, c := range dated {
		cum += c.Cost
		x := float64(c.TS-t0) / float64(t1-t0) * 300
		y := 58 - cum/total*56
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return sparkView{Points: b.String(), Max: fmt.Sprintf("$%.2f", total), OK: true}
}
