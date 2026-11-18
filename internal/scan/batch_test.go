package scan

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/tonym128/dashd/internal/model"
)

// fakeBatch records flushed sessions and can be told to fail.
type fakeBatch struct {
	pending  []model.SessionWrite
	flushed  []model.SessionWrite
	orphaned []string
	fail     bool
	size     int
}

func (f *fakeBatch) ReplaceSession(s model.SessionWrite) error {
	f.pending = append(f.pending, s)
	return nil
}
func (f *fakeBatch) AppendSession(s model.SessionWrite) error {
	f.pending = append(f.pending, s)
	return nil
}
func (f *fakeBatch) RecomputeSession(string) error { return nil }
func (f *fakeBatch) Pending() int                  { return len(f.pending) }
func (f *fakeBatch) BatchSize() int                { return f.size }
func (f *fakeBatch) MarkOrphaned(u []string) error { f.orphaned = append(f.orphaned, u...); return nil }
func (f *fakeBatch) Flush() error {
	if f.fail {
		f.pending = nil
		return errors.New("server down")
	}
	f.flushed = append(f.flushed, f.pending...)
	f.pending = nil
	return nil
}

func TestBatchSinkCursorsAdvanceOnlyAfterFlush(t *testing.T) {
	root := t.TempDir()
	for i := range 3 {
		writeJSONL(t, filepath.Join(root, fmt.Sprintf("s%d.jsonl", i)),
			fmt.Sprintf(`{"type":"session","id":"sess-%d","cwd":"/tmp/p"}`, i),
			`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"gemini-2.5-pro","usage":{"input":100,"output":50}}}`,
		)
	}
	st := newTestStore(t)
	sc := newScanner(t, st, root)
	sink := &fakeBatch{fail: true, size: 2}
	sc.cfg.Sink = sink

	if err := sc.RunOnce(context.Background()); err == nil {
		t.Fatal("a failed flush was not reported")
	}
	if n, _ := st.CountScanStates(); n != 0 {
		t.Fatalf("%d cursors saved despite the failed flush", n)
	}

	sink.fail = false
	if err := sc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.flushed) != 3 {
		t.Fatalf("flushed %d sessions, want all 3 resent", len(sink.flushed))
	}
	if n, _ := st.CountScanStates(); n != 3 {
		t.Fatalf("%d cursors saved, want 3", n)
	}
	// Nothing changed: nothing resent.
	sink.flushed = nil
	_ = sc.RunOnce(context.Background())
	if len(sink.flushed) != 0 {
		t.Fatalf("idle pass resent %d sessions", len(sink.flushed))
	}
}
