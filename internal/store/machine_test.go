package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
)

func TestMachinePersistenceAndFiltering(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ts := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	writeSession := func(uid, machine string, cost float64) {
		t.Helper()
		sess := model.SessionWrite{
			Session: model.Session{
				UID:     uid,
				Agent:   model.AgentClaude,
				Project: "/p/app",
				Machine: machine,
			},
			Path: "/p/app/" + uid + ".jsonl",
			Calls: []model.Call{{
				SessionUID:   uid,
				Agent:        model.AgentClaude,
				Project:      "/p/app",
				Machine:      machine,
				Model:        "claude-sonnet-4-6",
				CallKey:      "c1",
				Time:         ts,
				InputTokens:  1000,
				OutputTokens: 500,
				TotalTokens:  1500,
				CostUSD:      cost,
				Priced:       true,
			}},
			ToolCalls: []model.ToolCall{{
				SessionUID: uid,
				Agent:      model.AgentClaude,
				Project:    "/p/app",
				Machine:    machine,
				Tool:       "Bash",
				CallKey:    "t1",
				Time:       ts,
				Seconds:    1.5,
			}},
		}
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
	}

	writeSession("s-mac", "macbook", 1.25)
	writeSession("s-linux", "workstation", 2.50)

	// Append without explicit machine should inherit session.machine.
	if err := st.AppendSession(model.SessionWrite{
		Session: model.Session{UID: "s-mac", Agent: model.AgentClaude},
		Calls: []model.Call{{
			SessionUID:   "s-mac",
			Agent:        model.AgentClaude,
			Model:        "claude-sonnet-4-6",
			CallKey:      "c2",
			Time:         ts.Add(time.Minute),
			InputTokens:  500,
			OutputTokens: 250,
			TotalTokens:  750,
			CostUSD:      0.75,
			Priced:       true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("s-mac"); err != nil {
		t.Fatal(err)
	}

	machines, err := st.Machines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 2 || machines[0] != "macbook" || machines[1] != "workstation" {
		t.Fatalf("Machines() = %v, want [macbook workstation]", machines)
	}

	totMac, err := st.Totals(ctx, Filter{Machines: []string{"macbook"}})
	if err != nil {
		t.Fatal(err)
	}
	if totMac.Calls != 2 || totMac.Cost != 2.0 {
		t.Fatalf("Totals(macbook) = %+v, want calls=2 cost=2.0", totMac)
	}

	sessions, err := st.Sessions(ctx, Filter{Machines: []string{"workstation"}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Machine != "workstation" {
		t.Fatalf("Sessions(workstation) = %+v, want 1 session with machine=workstation", sessions)
	}
}
