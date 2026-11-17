package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/dashd/internal/model"
	"github.com/tonym128/dashd/internal/store"
)

func TestBuildInsightsFromStoreAndRender(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

	seed := func(uid, machine, project, modelName string, calls int, costEach float64, priced bool, longCtx bool, toolErrs int) {
		t.Helper()
		var callRows []model.Call
		var toolRows []model.ToolCall
		inTok := 1000
		if longCtx {
			inTok = 220000
		}
		for i := 0; i < calls; i++ {
			callRows = append(callRows, model.Call{
				SessionUID:      uid,
				Agent:           model.AgentClaude,
				Project:         project,
				Machine:         machine,
				Model:           modelName,
				CallKey:         fmtKey("c", i),
				Time:            now.Add(-time.Duration(i) * time.Minute),
				InputTokens:     inTok,
				OutputTokens:    500,
				CacheReadTokens: 100,
				ReasoningTokens: 400,
				TotalTokens:     inTok + 1000,
				LLMSeconds:      2.0,
				CostUSD:         costEach,
				Priced:          priced,
			})
		}
		for i := 0; i < 12; i++ {
			toolRows = append(toolRows, model.ToolCall{
				SessionUID: uid,
				Agent:      model.AgentClaude,
				Project:    project,
				Machine:    machine,
				Tool:       "Bash",
				CallKey:    fmtKey("t", i),
				Time:       now.Add(-time.Duration(i) * time.Minute),
				Seconds:    5.0,
				IsError:    i < toolErrs,
			})
		}
		if err := st.ReplaceSession(model.SessionWrite{
			Session:   model.Session{UID: uid, Agent: model.AgentClaude, Project: project, Machine: machine},
			Path:      project + "/" + uid + ".jsonl",
			Calls:     callRows,
			ToolCalls: toolRows,
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
	}

	seed("s1", "cachiest", "/p/big", "claude-opus-4-6", 45, 0.50, true, true, 4)
	seed("s2", "macbook", "/p/small", "claude-sonnet-4-6", 5, 0.05, true, false, 0)
	seed("s3", "cachiest", "/p/unpriced", "unknown-model-x", 2, 0, false, false, 0)

	insights, err := BuildInsightsFromStore(ctx, st, store.Filter{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) < 5 {
		t.Fatalf("expected >= 5 insights, got %d: %+v", len(insights), insights)
	}

	ids := map[string]bool{}
	for _, in := range insights {
		ids[in.ID] = true
	}
	for _, want := range []string{"unpriced-calls", "retry-loops", "tool-errors", "long-prompt-tier", "multi-machine"} {
		if !ids[want] {
			t.Errorf("missing expected insight %q (got %v)", want, ids)
		}
	}

	srv, err := New(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Generated = func() time.Time { return now }
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Key Insights") || !strings.Contains(body, "Usage Insights") {
		t.Errorf("expected rendered page to contain Key Insights and Usage Insights sections")
	}
}

func fmtKey(prefix string, i int) string {
	return prefix + string(rune('a'+i/26)) + string(rune('a'+i%26))
}
