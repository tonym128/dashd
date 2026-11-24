package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
)

func TestPriceHistoryKeepsWhatPastCallsCost(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old, change := time.Unix(1_700_000_000, 0), time.Unix(1_800_000_000, 0)
	if err := st.ReplaceSession(model.SessionWrite{Session: model.Session{UID: "u", Agent: "pi"},
		Calls: []model.Call{{SessionUID: "u", CallKey: "k", Agent: "pi", Model: "acme/x-1", Time: old}}}); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(dir, "models.json")
	write := func(body string) {
		if err := os.WriteFile(dump, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"data":[{"id":"acme/x-1","pricing":{"prompt":"0.000001","completion":"0"}}]}`)
	p, err := NewPricer(dump, "")
	if err != nil {
		t.Fatal(err)
	}
	cost := func(at time.Time) float64 {
		c, ok := p.CostCallAt(at, "acme/x-1", 1e6, 0, 0, 0, 0)
		if !ok {
			t.Fatal("unpriced")
		}
		return c
	}
	sync := func(now time.Time) {
		if err := p.SyncHistory(ctx, st, now); err != nil {
			t.Fatal(err)
		}
	}
	sync(old)
	// The price triples, and then the model is delisted.
	write(`{"data":[{"id":"acme/x-1","pricing":{"prompt":"0.000003","completion":"0"}}]}`)
	_ = p.ReloadOpenRouter(dump)
	sync(change)
	write(`{"data":[{"id":"acme/other","pricing":{"prompt":"0.000003","completion":"0"}}]}`)
	_ = p.ReloadOpenRouter(dump)
	sync(change.Add(time.Hour))
	if a, b := cost(old), cost(change.Add(time.Minute)); a != 1 || b != 3 {
		t.Errorf("before %v after %v, want 1 and 3", a, b)
	}

	// A price from now on leaves the past alone; one for all history does not.
	later := change.Add(2 * time.Hour)
	if err := st.SetManualPrice("x-1", `{"input":7}`, later, false); err != nil {
		t.Fatal(err)
	}
	sync(later)
	if a, b, c := cost(old), cost(change.Add(time.Minute)), cost(later); a != 1 || b != 3 || c != 7 {
		t.Errorf("%v %v %v, want 1 3 7", a, b, c)
	}
	// The catalogue does not supersede it.
	write(`{"data":[{"id":"acme/x-1","pricing":{"prompt":"0.000009","completion":"0"}}]}`)
	_ = p.ReloadOpenRouter(dump)
	sync(later.Add(time.Hour))
	if got := cost(later.Add(2 * time.Hour)); got != 7 {
		t.Errorf("catalogue overrode a manual price: %v", got)
	}
	if err := st.SetManualPrice("x-1", `{"input":5}`, later, true); err != nil {
		t.Fatal(err)
	}
	sync(later)
	if got := cost(old); got != 5 {
		t.Errorf("all-history price = %v, want 5", got)
	}
	if err := st.ClearPrices("x-1"); err != nil {
		t.Fatal(err)
	}
	sync(later)
	if got := cost(old); got != 9 {
		t.Errorf("after removal = %v, want the catalogue's 9", got)
	}
}
