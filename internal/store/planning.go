package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Budget is a monthly soft limit on one project, tag, model, machine or agent.
type Budget struct {
	Scope   string
	Name    string
	Monthly float64
}

// BudgetScopes are the axes a budget can be set on.
var BudgetScopes = []string{"project", "tag", "model", "machine", "agent"}

// Plan is a flat-fee subscription covering one agent.
type Plan struct {
	Name    string
	Agent   string
	Monthly float64
}

// Budgets lists the budgets.
func (s *Store) Budgets(ctx context.Context) ([]Budget, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT scope, name, monthly FROM budget ORDER BY scope, name`)
	if err != nil {
		return nil, fmt.Errorf("budgets: %w", err)
	}
	defer rows.Close()
	var out []Budget
	for rows.Next() {
		var b Budget
		if err := rows.Scan(&b.Scope, &b.Name, &b.Monthly); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBudget sets a budget, or removes it when the amount is 0.
func (s *Store) SetBudget(b Budget) error {
	return s.InTx(func(tx *sql.Tx) error {
		if b.Monthly <= 0 {
			_, err := tx.Exec(`DELETE FROM budget WHERE scope = ? AND name = ?`, b.Scope, b.Name)
			return err
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO budget (scope, name, monthly) VALUES (?,?,?)`, b.Scope, b.Name, b.Monthly)
		return err
	})
}

// Plans lists the subscriptions.
func (s *Store) Plans(ctx context.Context) ([]Plan, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT name, agent, monthly FROM plan ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("plans: %w", err)
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		var p Plan
		if err := rows.Scan(&p.Name, &p.Agent, &p.Monthly); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPlan sets a subscription, or removes it when the fee is 0.
func (s *Store) SetPlan(p Plan) error {
	return s.InTx(func(tx *sql.Tx) error {
		if p.Monthly <= 0 {
			_, err := tx.Exec(`DELETE FROM plan WHERE name = ?`, p.Name)
			return err
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO plan (name, agent, monthly) VALUES (?,?,?)`, p.Name, p.Agent, p.Monthly)
		return err
	})
}

// DimDay is one day's spend of one project, model and machine combination.
type DimDay struct {
	Day, Project, Model, Machine string
	Cost                         float64
}

// DimDays returns spend by day, project, model and machine since a day.
func (s *Store) DimDays(ctx context.Context, f Filter, since time.Time) ([]DimDay, error) {
	f.DateFrom, f.DateTo = &since, nil
	where, args := f.where("", "ts")
	rows, err := s.q.QueryContext(ctx, `
		SELECT day, project, model, machine, SUM(cost_usd) FROM call`+where+`
		GROUP BY day, project, model, machine`, args...)
	if err != nil {
		return nil, fmt.Errorf("dim days: %w", err)
	}
	defer rows.Close()
	var out []DimDay
	for rows.Next() {
		var d DimDay
		if err := rows.Scan(&d.Day, &d.Project, &d.Model, &d.Machine, &d.Cost); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// EachCallTokens calls fn with the billable tokens of every call in the filter.
// out includes reasoning.
func (s *Store) EachCallTokens(ctx context.Context, f Filter, fn func(model string, in, out, cacheRead, cacheWrite, cacheWrite1h int64, cost float64, priced bool)) error {
	where, args := f.where("", "ts")
	rows, err := s.q.QueryContext(ctx, `
		SELECT model, input_tokens, output_tokens + reasoning_tokens, cache_read_tokens,
		       cache_write_tokens, cache_write_1h_tokens, cost_usd, priced FROM call`+where, args...)
	if err != nil {
		return fmt.Errorf("call tokens: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		var in, out, cr, cw, cw1h int64
		var cost float64
		var priced int
		if err := rows.Scan(&m, &in, &out, &cr, &cw, &cw1h, &cost, &priced); err != nil {
			return err
		}
		fn(m, in, out, cr, cw, cw1h, cost, priced != 0)
	}
	return rows.Err()
}
