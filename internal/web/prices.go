package web

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tonym128/dashd/internal/source"
	"github.com/tonym128/dashd/internal/store"
)

// PriceStatus tracks the price catalogue's refreshes, for the Prices panel and
// the failing-refresh warning. The zero value is ready to use.
type PriceStatus struct {
	mu          sync.Mutex
	every       time.Duration
	lastAttempt time.Time
	lastSuccess time.Time
	lastError   string
	models      int
}

// Configure records how often the catalogue refreshes and how many models the
// loaded one holds.
func (p *PriceStatus) Configure(every time.Duration, models int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.every, p.models = every, models
}

// Attempt records the outcome of one refresh.
func (p *PriceStatus) Attempt(at time.Time, models int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastAttempt = at
	if err != nil {
		p.lastError = err.Error()
		return
	}
	p.lastError, p.lastSuccess, p.models = "", at, models
}

type priceView struct {
	Enabled     bool
	Every       string
	Models      int
	LastSuccess string
	LastError   string
	LastAttempt string
}

func (p *PriceStatus) view(now time.Time) priceView {
	if p == nil {
		return priceView{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ago := func(t time.Time) string {
		if t.IsZero() {
			return "not since this server started"
		}
		return humanAge(now.Sub(t))
	}
	return priceView{Enabled: true, Every: p.every.String(), Models: p.models,
		LastSuccess: ago(p.lastSuccess), LastError: p.lastError, LastAttempt: ago(p.lastAttempt)}
}

// PriceAdmin changes stored prices. cmd/dashd supplies it.
type PriceAdmin interface {
	SetPrice(ctx context.Context, model string, rates source.Rates, allHistory bool) (int, error)
	RemovePrice(ctx context.Context, model string) (int, error)
}

type priceChangeView struct {
	Model, From, Source         string
	Input, Output, Read, CacheW string
	Manual                      bool
}

// priceChanges lists the manual prices and the recorded changes, newest first.
// A catalogue price that has always applied is not a change.
func priceChanges(points []store.PricePoint) []priceChangeView {
	var out []priceChangeView
	for i := len(points) - 1; i >= 0; i-- {
		p := points[i]
		if p.ValidFrom == 0 && p.Source != "manual" {
			continue
		}
		var r source.Rates
		if json.Unmarshal([]byte(p.Rates), &r) != nil {
			continue
		}
		from := "always"
		if p.ValidFrom > 0 {
			from = displayTime(p.ValidFrom)
		}
		out = append(out, priceChangeView{Model: p.Model, From: from, Source: p.Source,
			Input: usd4(r.Input), Output: usd4(r.Output), Read: usd4(r.CacheRead), CacheW: usd4(r.CacheWrite),
			Manual: p.Source == "manual"})
	}
	return out
}

func usd4(v float64) string {
	s := strings.TrimRight(strings.TrimRight(groupDollars(sprintf4(v)), "0"), ".")
	return s
}

func (s *Server) handleAdminPrice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model      string  `json:"model"`
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
		AllHistory bool    `json:"all_history"`
		Remove     bool    `json:"remove"`
	}
	if !s.adminRequest(w, r, &req) {
		return
	}
	if s.PriceAdmin == nil {
		http.Error(w, "price editing not configured", http.StatusNotImplemented)
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" || len(req.Model) > 200 {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}
	for _, v := range []float64{req.Input, req.Output, req.CacheRead, req.CacheWrite} {
		if v < 0 || v > 1e6 || math.IsNaN(v) {
			http.Error(w, "prices are dollars per million tokens, 0 or more", http.StatusBadRequest)
			return
		}
	}
	var n int
	var err error
	if req.Remove {
		n, err = s.PriceAdmin.RemovePrice(r.Context(), req.Model)
	} else {
		n, err = s.PriceAdmin.SetPrice(r.Context(), req.Model, source.Rates{Input: req.Input, Output: req.Output,
			CacheRead: req.CacheRead, CacheWrite: req.CacheWrite}, req.AllHistory)
	}
	if err != nil {
		s.log.Error("price change failed", "model", req.Model, "error", err)
		http.Error(w, "price change failed", http.StatusInternalServerError)
		return
	}
	s.bumpGeneration()
	s.log.Info("admin changed a price", "model", req.Model, "removed", req.Remove, "calls", n)
	writeJSON(w, map[string]any{"ok": true, "repriced": n})
}
