package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

func writeSession(t *testing.T, st *Store, uid string, calls ...time.Time) {
	t.Helper()
	sess := model.SessionWrite{Session: model.Session{UID: uid, Agent: "claude", Project: "/p"}}
	for i, ts := range calls {
		sess.Calls = append(sess.Calls, model.Call{SessionUID: uid, Agent: "claude", Model: "m",
			CallKey: string(rune('a' + i)), Time: ts, Day: ts.Format("2006-01-02"),
			InputTokens: 10, TotalTokens: 10, CostUSD: 1, Priced: true})
	}
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession(uid); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRecomputesPartialSessionsAndSetsFloor(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old := time.Date(2025, 1, 5, 12, 0, 0, 0, time.Local)
	recent := time.Date(2026, 6, 5, 12, 0, 0, 0, time.Local)
	writeSession(t, st, "mixed", old, recent)

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	n, err := st.PruneBefore(ctx, cutoff, PruneOptions{})
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	s, ok, _ := st.Session(ctx, "mixed")
	if !ok || s.Calls != 1 || s.Cost != 1 {
		t.Fatalf("session after prune = calls %d cost %v; want its summary recomputed to 1/$1", s.Calls, s.Cost)
	}
	floor, _ := st.PruneFloor()
	if floor != cutoff.Unix() {
		t.Fatalf("floor = %d, want %d", floor, cutoff.Unix())
	}
	// A re-read of the same log must not resurrect the pruned call.
	writeSession(t, st, "mixed", old, recent)
	s, _, _ = st.Session(ctx, "mixed")
	if s.Calls != 1 {
		t.Fatalf("re-ingest brought pruned history back: %d calls", s.Calls)
	}
}

func TestPruneIncludeUndated(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(filepath.Join(t.TempDir(), "p.db"))
	defer st.Close()
	writeSession(t, st, "undated", time.Time{})
	if n, _ := st.PruneBefore(ctx, time.Now(), PruneOptions{}); n != 0 {
		t.Fatalf("undated call pruned without -include-undated")
	}
	if n, _ := st.PruneBefore(ctx, time.Now(), PruneOptions{IncludeUndated: true}); n != 1 {
		t.Fatalf("undated call not pruned with IncludeUndated")
	}
}

func TestRepriceCountsOnlyChangedRowsAndRebuckets(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(filepath.Join(t.TempDir(), "p.db"))
	defer st.Close()
	ts := time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)
	writeSession(t, st, "s", ts, ts.Add(time.Hour))
	same := func(string, int64, int64, int64, int64, int64) (float64, bool) { return 1, true }
	if n, err := st.Reprice(ctx, same); err != nil || n != 0 {
		t.Fatalf("unchanged reprice = %d, %v; want 0", n, err)
	}
	if _, err := st.DB().Exec(`UPDATE call SET day = '1999-01-01'`); err != nil {
		t.Fatal(err)
	}
	double := func(string, int64, int64, int64, int64, int64) (float64, bool) { return 2, true }
	if n, _ := st.Reprice(ctx, double); n != 2 {
		t.Fatalf("changed reprice = %d, want 2", n)
	}
	var day string
	_ = st.DB().QueryRow(`SELECT day FROM call LIMIT 1`).Scan(&day)
	if day != "2026-03-01" {
		t.Fatalf("day = %s, want re-derived from ts", day)
	}
	s, _, _ := st.Session(ctx, "s")
	if s.Cost != 4 {
		t.Fatalf("session cost = %v, want 4 after reprice", s.Cost)
	}
}

func TestResolveIngestUIDAndInstanceID(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "p.db"))
	defer st.Close()
	sess := model.SessionWrite{Session: model.Session{UID: "u", Agent: "claude", Machine: "a"}}
	_ = st.ReplaceSession(sess)
	for machine, want := range map[string]string{"a": "u", "b": "b::u", "": "u"} {
		got, err := st.ResolveIngestUID("u", machine)
		if err != nil || got != want {
			t.Errorf("ResolveIngestUID(u, %q) = %q, %v; want %q", machine, got, err, want)
		}
	}
	id1, _ := st.InstanceID()
	id2, _ := st.InstanceID()
	if id1 == "" || id1 != id2 {
		t.Fatalf("instance id unstable: %q %q", id1, id2)
	}
}

func TestBackupAndRename(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := Open(filepath.Join(dir, "p.db"))
	defer st.Close()
	writeSession(t, st, "s", time.Now())
	dest := filepath.Join(dir, "copy.db")
	if err := st.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := st.Backup(ctx, dest); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	cp, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	tot, _ := cp.Totals(ctx, Filter{})
	cp.Close()
	if tot.Calls != 1 {
		t.Fatalf("backup has %d calls", tot.Calls)
	}
	if fi, _ := os.Stat(dest); fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %o, want 600", fi.Mode().Perm())
	}
	if _, err := st.RenameMachine("", "x"); err == nil {
		t.Error("rename accepted an empty name")
	}
}

func TestHeartbeatRecordsErrors(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(filepath.Join(t.TempDir(), "p.db"))
	defer st.Close()
	now := time.Now()
	_ = st.RecordMachineHeartbeat(Heartbeat{Machine: "m", At: now, Error: "boom", Version: "1.2.3"})
	ms, _ := st.MachineStatuses(ctx)
	if len(ms) != 1 || ms[0].LastError != "boom" || ms[0].Version != "1.2.3" || ms[0].LastErrorAt == 0 {
		t.Fatalf("status = %+v", ms)
	}
	_ = st.RecordMachineHeartbeat(Heartbeat{Machine: "m", At: now})
	ms, _ = st.MachineStatuses(ctx)
	if ms[0].LastError != "" || ms[0].Version != "1.2.3" {
		t.Fatalf("healthy heartbeat did not clear the error or dropped the version: %+v", ms[0])
	}
}

func TestSessionsPageSearchSortAndPaging(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(filepath.Join(t.TempDir(), "p.db"))
	defer st.Close()
	for i := range 7 {
		uid := string(rune('a' + i))
		writeSession(t, st, uid, time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.Local))
	}
	_, _ = st.DB().Exec(`UPDATE session SET title = 'needle 50%' WHERE uid = 'c'`)
	rows, total, err := st.SessionsPage(ctx, Filter{}, SessionQuery{Limit: 3, Offset: 3})
	if err != nil || total != 7 || len(rows) != 3 || rows[0].UID != "d" {
		t.Fatalf("page = %d rows, total %d, first %v, err %v", len(rows), total, rows, err)
	}
	rows, total, _ = st.SessionsPage(ctx, Filter{}, SessionQuery{Search: "50%"})
	if total != 1 || rows[0].UID != "c" {
		t.Fatalf("search = %d %v", total, rows)
	}
	rows, _, _ = st.SessionsPage(ctx, Filter{}, SessionQuery{Sort: "last", Asc: true, Limit: 1})
	if rows[0].UID != "a" {
		t.Fatalf("asc sort first = %s", rows[0].UID)
	}
	rows, _, _ = st.SessionsPage(ctx, Filter{}, SessionQuery{Sort: "x; DROP TABLE call", Limit: 1})
	if len(rows) != 1 {
		t.Fatal("unknown sort key broke the query")
	}
}

func TestMigrationBacksUpOldDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.db")
	st, _ := Open(path)
	writeSession(t, st, "s", time.Now())
	if _, err := st.DB().Exec(`PRAGMA user_version = 8`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := os.Stat(path + ".bak-v8"); err != nil {
		t.Fatalf("no pre-migration backup: %v", err)
	}
}

func TestMigrationV10DropsCursorsForUnknownAntigravityModels(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	_ = st.ReplaceSession(model.SessionWrite{Session: model.Session{UID: "c1", Agent: "agy"},
		Calls: []model.Call{{SessionUID: "c1", Agent: "agy", Model: "antigravity-model-1526", CallKey: "k"}}})
	_ = st.ReplaceSession(model.SessionWrite{Session: model.Session{UID: "c2", Agent: "agy"},
		Calls: []model.Call{{SessionUID: "c2", Agent: "agy", Model: "gemini-4-argon", CallKey: "k"}}})
	for _, uid := range []string{"c1", "c2"} {
		if err := st.SaveScanState(model.ScanState{Path: "/x/" + uid + ".db", Agent: "agy", SessionUID: uid, Offset: 9}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.InTx(v10RereadUnknownAntigravity.apply); err != nil {
		t.Fatal(err)
	}
	states, _ := st.LoadScanStates()
	if _, ok := states["/x/c1.db"]; ok {
		t.Error("cursor kept for a conversation with an unknown model")
	}
	if _, ok := states["/x/c2.db"]; !ok {
		t.Error("cursor dropped for a conversation whose models are known")
	}
}
