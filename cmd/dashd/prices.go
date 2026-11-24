package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tonym128/dashd/internal/source"
	"github.com/tonym128/dashd/internal/store"
)

// priceAdmin applies hand-entered prices and reprices what they touch.
type priceAdmin struct {
	st     *store.Store
	pricer *source.Pricer
}

func (a priceAdmin) SetPrice(ctx context.Context, model string, rates source.Rates, allHistory bool) (int, error) {
	raw, err := json.Marshal(rates)
	if err != nil {
		return 0, err
	}
	if err := a.st.SetManualPrice(source.NormalizeModel(model), string(raw), time.Now(), allHistory); err != nil {
		return 0, err
	}
	return a.apply(ctx)
}

func (a priceAdmin) RemovePrice(ctx context.Context, model string) (int, error) {
	if err := a.st.ClearPrices(source.NormalizeModel(model)); err != nil {
		return 0, err
	}
	return a.apply(ctx)
}

func (a priceAdmin) apply(ctx context.Context) (int, error) {
	if err := a.pricer.SyncHistory(ctx, a.st, time.Now()); err != nil {
		return 0, err
	}
	return a.st.Reprice(ctx, priceAt(a.pricer))
}
