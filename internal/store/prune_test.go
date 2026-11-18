package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

func TestPruneBeforeRemovesOldCallsAndEmptySessions(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	oldTS := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	newTS := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		uid string
		ts  time.Time
	}{
		{"old-sess", oldTS},
		{"new-sess", newTS},
	} {
		if err := st.ReplaceSession(model.SessionWrite{
			Session: model.Session{UID: tc.uid, Agent: model.AgentClaude, Project: "/p/app"},
			Path:    "/p/app/" + tc.uid + ".jsonl",
			Calls: []model.Call{{
				SessionUID:   tc.uid,
				Agent:        model.AgentClaude,
				Project:      "/p/app",
				Model:        "claude-sonnet-4-6",
				CallKey:      "c1",
				Time:         tc.ts,
				InputTokens:  100,
				OutputTokens: 50,
				TotalTokens:  150,
				CostUSD:      0.10,
				Priced:       true,
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(tc.uid); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := st.PruneBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneBefore: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}

	tot, err := st.Totals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if tot.Calls != 1 || tot.Sessions != 1 {
		t.Fatalf("after prune totals = %+v, want calls=1 sessions=1", tot)
	}
}
