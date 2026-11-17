package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestUpdateModelsAndReloadOpenRouter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "acme/new-test-model-99",
					"pricing": {"prompt": "0.000003", "completion": "0.000015"}
				}
			]
		}`))
	}))
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "sub", "models.json")
	n, err := UpdateModels(context.Background(), ts.URL, dest)
	if err != nil {
		t.Fatalf("UpdateModels: %v", err)
	}
	if n != 1 {
		t.Fatalf("UpdateModels count = %d, want 1", n)
	}

	pricer, err := NewFallbackPricer()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pricer.Resolve("acme/new-test-model-99"); ok {
		t.Fatal("model should not resolve before ReloadOpenRouter")
	}
	if err := pricer.ReloadOpenRouter(dest); err != nil {
		t.Fatalf("ReloadOpenRouter: %v", err)
	}
	r, ok := pricer.Resolve("acme/new-test-model-99")
	if !ok {
		t.Fatal("model did not resolve after ReloadOpenRouter")
	}
	if r.Input != 3.0 || r.Output != 15.0 {
		t.Fatalf("rates = %+v, want input=3.0 output=15.0", r)
	}
}
