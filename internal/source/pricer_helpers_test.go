package source

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPricerCatalogueHelpers(t *testing.T) {
	fb, err := NewFallbackPricer()
	if err != nil {
		t.Fatal(err)
	}
	if !fb.FallbackOnly() || fb.LiveCatalogueSize() != 0 || fb.FallbackSize() == 0 {
		t.Fatalf("fallback pricer: only=%v live=%d size=%d", fb.FallbackOnly(), fb.LiveCatalogueSize(), fb.FallbackSize())
	}
	if len(fb.LiveModelKeys()) != fb.FallbackSize() {
		t.Fatalf("keys %d != fallback size %d", len(fb.LiveModelKeys()), fb.FallbackSize())
	}
	dir := t.TempDir()
	models := filepath.Join(dir, "models.json")
	_ = os.WriteFile(models, []byte(`{"data":[{"id":"acme/zeta-1","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`), 0o644)
	live, err := NewPricer(models, "")
	if err != nil {
		t.Fatal(err)
	}
	if live.FallbackOnly() || live.LiveCatalogueSize() == 0 || len(live.LiveModelKeys()) <= fb.FallbackSize() {
		t.Fatalf("live pricer: only=%v live=%d keys=%d", live.FallbackOnly(), live.LiveCatalogueSize(), len(live.LiveModelKeys()))
	}
	etag := ModelsFileETag(models)
	if len(etag) != 64 || ModelsFileETag("") != "" || ModelsFileETag(filepath.Join(dir, "nope")) != "" {
		t.Fatalf("etag = %q", etag)
	}
	if NewAntigravityParser().Agent() != "agy" {
		t.Fatal("agy parser agent")
	}
}
