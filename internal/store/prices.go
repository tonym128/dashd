package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PricePoint is one entry of a model's price history. Rates is JSON, owned by
// the pricing package.
type PricePoint struct {
	Model     string
	ValidFrom int64
	Rates     string
	Source    string // "catalogue" or "manual"
}

// PriceHistory returns every recorded price, oldest first per model.
func (s *Store) PriceHistory(ctx context.Context) ([]PricePoint, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT model, valid_from, rates, source FROM price_history ORDER BY model, valid_from`)
	if err != nil {
		return nil, fmt.Errorf("price history: %w", err)
	}
	defer rows.Close()
	var out []PricePoint
	for rows.Next() {
		var p PricePoint
		if err := rows.Scan(&p.Model, &p.ValidFrom, &p.Rates, &p.Source); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordPrices adds points to the history.
func (s *Store) RecordPrices(points []PricePoint) error {
	if len(points) == 0 {
		return nil
	}
	return s.InTx(func(tx *sql.Tx) error {
		for _, p := range points {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO price_history (model, valid_from, rates, source) VALUES (?,?,?,?)`,
				p.Model, p.ValidFrom, p.Rates, p.Source); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetManualPrice records a hand-entered price. With allHistory it replaces
// everything known about the model; otherwise it applies from at.
func (s *Store) SetManualPrice(model, rates string, at time.Time, allHistory bool) error {
	return s.InTx(func(tx *sql.Tx) error {
		from := at.Unix()
		if allHistory {
			from = 0
			if _, err := tx.Exec(`DELETE FROM price_history WHERE model = ?`, model); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO price_history (model, valid_from, rates, source) VALUES (?,?,?,'manual')`, model, from, rates)
		return err
	})
}

// ClearPrices forgets a model's history, or every catalogue price when model is
// empty, so the next sync records the current prices as always having applied.
func (s *Store) ClearPrices(model string) error {
	q, args := `DELETE FROM price_history WHERE source <> 'manual'`, []any{}
	if model != "" {
		q, args = `DELETE FROM price_history WHERE model = ?`, []any{model}
	}
	return s.InTx(func(tx *sql.Tx) error { _, err := tx.Exec(q, args...); return err })
}
