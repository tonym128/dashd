package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

func TestStoreExtras(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()

	items := []IngestItem{
		{Replace: true, Session: model.SessionWrite{
			Session:   model.Session{UID: "a", Agent: "claude", Project: "/p", Machine: "m1"},
			Calls:     []model.Call{{SessionUID: "a", Agent: "claude", Model: "x", CallKey: "1", Time: now, CostUSD: 2, Priced: true}},
			ToolCalls: []model.ToolCall{{SessionUID: "a", Agent: "claude", Tool: "Bash", CallKey: "t1", Time: now, Seconds: 1.5, IsError: true}},
		}},
		{Replace: false, Session: model.SessionWrite{
			Session: model.Session{UID: "b", Agent: "pi", Machine: "m2"},
			Calls:   []model.Call{{SessionUID: "b", Agent: "pi", Model: "y", CallKey: "1", Time: now, CostUSD: 3, Priced: true}},
		}},
	}
	if err := st.ApplyIngest(items, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	b, ok, _ := st.Session(ctx, "b")
	if !ok || !b.Orphaned || b.Cost != 3 {
		t.Fatalf("session b = %+v", b)
	}

	var n int
	if err := st.ExportCalls(ctx, Filter{Machines: []string{"m1"}}, func(c ExportCall) error {
		n++
		if c.Model != "x" || !c.Priced || c.IngestedAt == 0 {
			t.Errorf("export row = %+v", c)
		}
		return nil
	}); err != nil || n != 1 {
		t.Fatalf("export = %d, %v", n, err)
	}

	tools, err := st.SessionTools(ctx, "a")
	if err != nil || len(tools) != 1 || !tools[0].IsError || tools[0].Tool != "Bash" {
		t.Fatalf("tools = %+v %v", tools, err)
	}
	if c, _ := st.CostSince(ctx, now.Add(-time.Minute).Unix()); c != 5 {
		t.Fatalf("CostSince = %v", c)
	}
	by, err := st.CostBy(ctx, "machine")
	if err != nil || by["m1"] != 2 || by["m2"] != 3 {
		t.Fatalf("CostBy = %v %v", by, err)
	}
	if _, err := st.CostBy(ctx, "model; drop"); err == nil {
		t.Fatal("CostBy accepted an arbitrary column")
	}
	if ok, _ := st.SessionExists("a"); !ok {
		t.Fatal("SessionExists(a) = false")
	}
	if ok, _ := st.SessionExists("zz"); ok {
		t.Fatal("SessionExists(zz) = true")
	}

	_ = st.SaveScanState(model.ScanState{Path: "/f", Agent: "claude", Offset: 1})
	if c, _ := st.CountScanStates(); c != 1 {
		t.Fatalf("CountScanStates = %d", c)
	}
	if err := st.ResetScanStates(); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.CountScanStates(); c != 0 {
		t.Fatalf("after reset = %d", c)
	}
	if err := st.SetMeta("k", "v1"); err != nil {
		t.Fatal(err)
	}
	_ = st.SetMeta("k", "v2")
	if v, _ := st.Meta("k"); v != "v2" {
		t.Fatalf("meta = %q", v)
	}
	if v, _ := st.Meta("missing"); v != "" {
		t.Fatalf("missing meta = %q", v)
	}
	if f, _ := st.PruneFloor(); f != 0 {
		t.Fatalf("floor = %d", f)
	}
}

func TestScanStatusAndBulkHelpers(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(filepath.Join(t.TempDir(), "y.db"))
	defer st.Close()
	if err := st.RecordScanStatus("claude", model.ScanStatus{Agent: "claude", LastScanAt: time.Now(), FilesSeen: 3, Error: "x"}); err != nil {
		t.Fatal(err)
	}
	ss, err := st.ScanStatuses()
	if err != nil || len(ss) != 1 || ss[0].FilesSeen != 3 || ss[0].Error != "x" {
		t.Fatalf("statuses = %+v %v", ss, err)
	}
	_ = st.SaveScanState(model.ScanState{Path: "/a", Agent: "claude"})
	_ = st.SaveScanState(model.ScanState{Path: "/b", Agent: "claude"})
	if err := st.ForgetScanStates([]string{"/a"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountScanStates(); n != 1 {
		t.Fatalf("after forget = %d", n)
	}
	writeSession(t, st, "s", time.Now())
	_, _ = st.DB().Exec(`UPDATE session SET cost_usd = 99`)
	if err := st.RecomputeAllSessions(); err != nil {
		t.Fatal(err)
	}
	if err := st.ReadTx(ctx, func(tx *Store) error {
		s, ok, err := tx.Session(ctx, "s")
		if !ok || s.Cost != 1 {
			t.Errorf("recomputed session = %+v", s)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
