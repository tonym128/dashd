package source

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/tonym128/dashd/internal/store"
)

// PriceStore is the part of the store a price book needs.
type PriceStore interface {
	PriceHistory(ctx context.Context) ([]store.PricePoint, error)
	RecordPrices(points []store.PricePoint) error
	Facets(ctx context.Context) (models, agents, projects []string, minTS, maxTS int64, err error)
}

type pricePoint struct {
	from  int64
	rates Rates
}

// SyncHistory records the current price of every model in the database and
// loads the history back into the pricer.
//
// The first price seen for a model is taken to have always applied; a later one
// applies from now. That is what stops a change in the catalogue, or a model
// leaving it, from rewriting what past calls cost. A hand-entered price is never
// superseded by the catalogue.
func (p *Pricer) SyncHistory(ctx context.Context, st PriceStore, now time.Time) error {
	points, err := st.PriceHistory(ctx)
	if err != nil {
		return err
	}
	models, _, _, _, _, err := st.Facets(ctx)
	if err != nil {
		return err
	}
	last := map[string]store.PricePoint{}
	for _, pt := range points {
		last[pt.Model] = pt
	}
	var fresh []store.PricePoint
	seen := map[string]bool{}
	for _, m := range models {
		key := NormalizeModel(m)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		rates, ok := p.Resolve(m)
		if !ok {
			continue
		}
		raw, err := json.Marshal(rates)
		if err != nil {
			return err
		}
		prev, known := last[key]
		if known && (prev.Source == "manual" || prev.Rates == string(raw)) {
			continue
		}
		pt := store.PricePoint{Model: key, Rates: string(raw), Source: "catalogue"}
		if known {
			pt.ValidFrom = now.Unix()
		}
		fresh = append(fresh, pt)
	}
	if err := st.RecordPrices(fresh); err != nil {
		return err
	}
	hist := map[string][]pricePoint{}
	for _, pt := range append(points, fresh...) {
		var r Rates
		if err := json.Unmarshal([]byte(pt.Rates), &r); err != nil {
			continue
		}
		hist[pt.Model] = append(hist[pt.Model], pricePoint{pt.ValidFrom, r})
	}
	for _, h := range hist {
		sort.SliceStable(h, func(i, j int) bool { return h[i].from < h[j].from })
	}
	p.mu.Lock()
	p.history = hist
	p.mu.Unlock()
	return nil
}

// ratesAt returns the rates a call made at a moment pays: from the recorded
// history when the model has one, otherwise the current price.
func (p *Pricer) ratesAt(model string, at time.Time) (Rates, bool) {
	p.mu.RLock()
	h := p.history[NormalizeModel(model)]
	p.mu.RUnlock()
	if len(h) == 0 {
		r, ok := p.Resolve(model)
		return r.at(at), ok
	}
	if at.IsZero() {
		return h[0].rates.at(at), true
	}
	found := -1
	for i, pt := range h {
		if pt.from <= at.Unix() {
			found = i
		}
	}
	if found < 0 {
		// Older than a price entered to apply from some day on.
		r, ok := p.Resolve(model)
		return r.at(at), ok
	}
	return h[found].rates.at(at), true
}
