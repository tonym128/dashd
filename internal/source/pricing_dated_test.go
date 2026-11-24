package source

import (
	"testing"
	"time"
)

func TestPriceChangeAppliesFromItsDate(t *testing.T) {
	p, err := NewFallbackPricer()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"gemini-3.6-flash", "gemini-3.8-flash"} {
		for _, tc := range []struct {
			at   time.Time
			want float64
		}{
			{time.Time{}, 0.75 + 3.75 + 0.075},
			{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), 0.75 + 3.75 + 0.075},
			{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 1.5 + 7.5 + 0.15},
		} {
			got, ok := p.CostCallAt(tc.at, m, 1e6, 1e6, 1e6, 0, 0)
			if d := got - tc.want; !ok || d > 1e-9 || d < -1e-9 {
				t.Errorf("%s at %v = %v, %v; want %v", m, tc.at, got, ok, tc.want)
			}
		}
	}
}

func TestFreeModelsArePricedAtZero(t *testing.T) {
	p, err := NewFallbackPricer()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"big-pickle", "bonsai-2-27b", "gemma4:12b", "qwen3-vl-8b",
		"qwen3.8-flash-next-iq3_s", "deepseek-v4.1-flash-free", "nemotron-3.5-super-vl-free",
		"gpt-5.4-free", "ggml-org/gemma-3-12b-it-qat-GGUF", "Qwen3.8-27B-GGUF-UD-Q2_K_XL"} {
		if cost, ok := p.Cost(m, 1e6, 1e6, 0, 0); !ok || cost != 0 {
			t.Errorf("%s = %v, %v; want free", m, cost, ok)
		}
	}
	for _, m := range []string{"freedom-1", "qwen3-vl-8b-instruct", "gemma4:12b-cloud"} {
		if _, ok := p.Resolve(m); ok {
			t.Errorf("%s resolved; want unpriced", m)
		}
	}
	if cost, _ := p.Cost("gpt-5.4", 1e6, 0, 0, 0); cost == 0 {
		t.Error("gpt-5.4 is free")
	}
}
